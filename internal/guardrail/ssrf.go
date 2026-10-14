package guardrail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ssrfLookupTimeout bounds every DNS lookup made for SSRF validation so an
// unresponsive resolver can never hang an admin request or an outbound dial.
// It is a var (not const) ONLY so tests can shorten it; production never
// reassigns it.
var ssrfLookupTimeout = 5 * time.Second

// lookupIPAddr is the resolver seam used by resolveAndCheck. It exists so
// tests can inject deterministic resolutions (mixed A/AAAA sets, rebinding,
// failures). Production code never reassigns it.
var lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// isHardDeniedIP reports the SSRF red line: address classes that are NEVER
// reachable, regardless of any operator allowlist — loopback (v4+v6,
// including IPv4-mapped IPv6 after normalization), link-local unicast and
// multicast (this covers the classic 169.254.0.0/16 cloud metadata range),
// the unspecified address, multicast, AND the explicit cloud-metadata
// targets below that do NOT live in link-local space. Allowlist entries can
// never override this; the allowlist validator additionally rejects any
// entry overlapping these blocks, so they cannot even be configured.
func isHardDeniedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || isCloudMetadataIP(ip)
}

// cloudMetadataPrefixes are metadata-service targets that are NOT covered by
// the link-local ranges and therefore need explicit, unconditional denial:
//
//   - fd00:ec2::/32 — AWS reserves this entire block for EC2 instance
//     infrastructure; the IPv6 instance metadata service answers at
//     fd00:ec2::254 (AWS docs: instance metadata data retrieval). The block
//     sits INSIDE the ULA range fc00::/7, so without this explicit rule a
//     wide ULA allowlist would open the metadata service. The whole /32 is
//     denied (not just the /128) because AWS reserves it for infrastructure:
//     no operator-owned legitimate target can live inside it.
//   - 100.100.100.200/32 — Alibaba Cloud ECS metadata endpoint, which lives
//     in carrier-grade NAT space (100.64.0.0/10), i.e. neither RFC1918
//     private nor link-local. Only the exact /32 is denied; adjacent
//     addresses are unaffected.
//
// The IPv4 metadata endpoints of AWS/GCP/Azure/OpenStack/DigitalOcean all
// use 169.254.169.254, already hard-denied as link-local.
var cloudMetadataPrefixes = []netip.Prefix{
	netip.MustParsePrefix("fd00:ec2::/32"),
	netip.MustParsePrefix("100.100.100.200/32"),
}

func isCloudMetadataIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range cloudMetadataPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func isRestrictedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// internalAllowlist holds the operator-configured set of restricted CIDRs that
// outbound SSRF-safe connections are nonetheless permitted to reach (e.g. an
// on-prem deployment's VPC range, so self-hosted LLM providers on 10.x work).
// nil means "block all restricted ranges" — the default, most-secure posture.
// Read on every dial via atomic load (lock-free); set once at startup.
var internalAllowlist atomic.Pointer[internalCIDRSet]

type internalCIDRSet struct {
	prefixes []netip.Prefix
}

// privateSupernets are the ONLY ranges an allowlist entry may cover. Anything
// else — loopback, link-local/cloud-metadata, unspecified, multicast, public
// space, or a CIDR wider than a private supernet — is rejected at
// configuration time. Red-line classes are disjoint from these supernets, so a
// valid allowlist can never widen into them.
var privateSupernets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// validateAllowlistPrefix enforces that one allowlist entry is a subnet of a
// private supernet AND does not overlap the cloud-metadata blocks. IPv4-mapped
// IPv6 prefixes are rejected with a pointer to the IPv4 form (mask semantics
// across the mapping are error-prone; the plain IPv4 spelling is always
// available).
func validateAllowlistPrefix(p netip.Prefix) error {
	if p.Addr().Is4In6() {
		return fmt.Errorf("ssrf: allowlist CIDR %s is an IPv4-mapped IPv6 prefix; use the IPv4 form %s/%d",
			p, p.Addr().Unmap(), p.Bits()-96)
	}
	masked := p.Masked()
	for _, supernet := range privateSupernets {
		if supernet.Contains(masked.Addr()) && p.Bits() >= supernet.Bits() {
			for _, meta := range cloudMetadataPrefixes {
				if meta.Overlaps(masked) {
					return fmt.Errorf("ssrf: allowlist CIDR %s overlaps cloud-metadata block %s (unconditionally denied)", masked, meta)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("ssrf: allowlist CIDR %s is not a subnet of a private range (allowed supernets: 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7); loopback, link-local/cloud-metadata, unspecified and multicast addresses are never allowlistable", p)
}

// ValidateInternalAllowlist is the PURE validation half of the allowlist
// policy: it parses and policy-checks every CIDR entry (same rules as
// SetInternalAllowlist) and returns the normalized prefix set WITHOUT
// touching the active policy. Callers that persist a configuration (e.g.
// the B5 admin hot-reload flow) MUST validate here first and only publish
// the validated prefixes via ApplyInternalAllowlist after the store
// committed — never mutate memory before persistence.
func ValidateInternalAllowlist(cidrs []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("ssrf: invalid allowlist CIDR %q: %w", c, err)
		}
		if err := validateAllowlistPrefix(p); err != nil {
			return nil, err
		}
		prefixes = append(prefixes, p)
	}
	return prefixes, nil
}

// SetInternalAllowlist configures the restricted CIDRs outbound SSRF-safe
// connections may reach, in addition to all public addresses. Pass an empty
// slice to (re)apply the strict default. Every entry must validate; on any
// error NOTHING is applied and the previously active allowlist is left
// unchanged (all-or-nothing: an invalid configuration can never partially
// widen the policy).
func SetInternalAllowlist(cidrs []string) error {
	prefixes, err := ValidateInternalAllowlist(cidrs)
	if err != nil {
		return err
	}
	ApplyInternalAllowlist(prefixes)
	return nil
}

// ApplyInternalAllowlist atomically publishes an ALREADY-VALIDATED prefix
// set (see ValidateInternalAllowlist — it performs no validation itself).
// An empty slice re-applies the strict default. Registered observers (see
// OnInternalAllowlistApply) run after the swap so connection pools can drop
// idle connections that the previous, wider policy allowed.
func ApplyInternalAllowlist(prefixes []netip.Prefix) {
	if len(prefixes) == 0 {
		internalAllowlist.Store(nil)
	} else {
		internalAllowlist.Store(&internalCIDRSet{prefixes: prefixes})
	}
	// Revocation order matters: first cut the live connections whose IP the
	// new policy blocks (busy or idle — pool reuse must never bypass the
	// policy), then let observers drop remaining idle pooled connections.
	closeRevokedConns()
	notifyAllowlistObservers()
}

// InternalAllowlistSnapshot returns the currently active prefixes (nil/empty
// = strict default) for status reporting. Read-only; treat as immutable.
func InternalAllowlistSnapshot() []netip.Prefix {
	set := internalAllowlist.Load()
	if set == nil {
		return nil
	}
	return set.prefixes
}

// allowlistObservers track state that must react to a policy swap (idle
// pooled connections the old policy allowed). Registration is append-only;
// entries are cheap function closures over long-lived transports.
var (
	allowlistObserversMu sync.Mutex
	allowlistObservers   []func()
)

// OnInternalAllowlistApply registers an observer invoked after every
// successful ApplyInternalAllowlist / SetInternalAllowlist. Observers must
// be cheap and non-blocking (they run inline on the publishing goroutine).
func OnInternalAllowlistApply(fn func()) {
	if fn == nil {
		return
	}
	allowlistObserversMu.Lock()
	allowlistObservers = append(allowlistObservers, fn)
	allowlistObserversMu.Unlock()
}

func notifyAllowlistObservers() {
	allowlistObserversMu.Lock()
	fns := make([]func(), len(allowlistObservers))
	copy(fns, allowlistObservers)
	allowlistObserversMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// AllowlistHit describes one allowlist admission for audit: the ACTUAL
// resolved target IP and the exact allowlist CIDR that admitted it. No URL,
// credentials, query or body content is ever carried here.
type AllowlistHit struct {
	IP   string
	CIDR string
}

// allowlistHitFor reports the matching allowlist prefix for an IP, if any.
func allowlistHitFor(ip net.IP) (netip.Prefix, bool) {
	set := internalAllowlist.Load()
	if set == nil {
		return netip.Prefix{}, false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Prefix{}, false
	}
	addr = addr.Unmap()
	for _, p := range set.prefixes {
		if p.Contains(addr) {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// AllowedInternal reports whether ip is an internal/restricted address that the
// operator has explicitly allowlisted. Public addresses are never routed here.
func AllowedInternal(ip net.IP) bool {
	_, ok := allowlistHitFor(ip)
	return ok
}

// blockedIP is the connection policy. The SSRF red line (loopback,
// link-local/cloud-metadata, unspecified, multicast — including their IPv6 and
// IPv4-mapped IPv6 forms) is denied FIRST and cannot be overridden by any
// allowlist; only the private ranges are soft-restricted and thus
// allowlistable.
func blockedIP(ip net.IP) bool {
	if isHardDeniedIP(ip) {
		return true
	}
	return isRestrictedIP(ip) && !AllowedInternal(ip)
}

// ssrfControl validates the actual destination IP at the socket Control layer,
// blocking restricted addresses to prevent DNS rebinding between resolution and
// connection. Shared by the plain and TLS SSRF-safe dialers.
func ssrfControl(network, address string, _ syscall.RawConn) error {
	h, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return fmt.Errorf("ssrf: non-IP destination %q", h)
	}
	if blockedIP(ip) {
		return fmt.Errorf("ssrf: blocked connection to restricted address %s", ip)
	}
	return nil
}

// auditAllowlistedDial records that one connection reached a restricted-but-
// operator-allowlisted address. One record per connection (never per token/
// chunk), at the DEFAULT-VISIBLE Info level, carrying the IP and the matching
// CIDR only — no URL, credentials, query or body content. Hard-denied and
// public addresses are never recorded here; refusals produce no admission
// record at all.
func auditAllowlistedDial(ip net.IP) {
	if isHardDeniedIP(ip) {
		return
	}
	if p, ok := allowlistHitFor(ip); ok {
		slog.Info("ssrf allowlist: connection admitted", "ip", ip.String(), "cidr", p.String())
	}
}

// outboundErr classifies an admission failure so operator-facing callers can
// report the REAL reason (malformed URL vs unresolvable host vs restricted
// target) instead of a single misleading message. The wire message stays
// byte-identical to the historical text; kind is "restricted", "unresolved"
// or "invalid"; host carries only the hostname or IP literal as typed —
// never a full URL, credentials, query or body.
type outboundErr struct {
	kind string
	host string
	msg  string
}

func (e *outboundErr) Error() string { return e.msg }

// OutboundErrorInfo reports the classification of an admission failure
// returned by ValidateURLSafety/ValidateURLSafetyDetail. ok is false for
// unrelated errors.
func OutboundErrorInfo(err error) (kind, host string, ok bool) {
	var oe *outboundErr
	if errors.As(err, &oe) {
		return oe.kind, oe.host, true
	}
	return "", "", false
}

// resolveAndCheck resolves host and returns an error if ANY resolved address
// would be blocked (conservative: a name with one forbidden address among its
// A/AAAA set is rejected outright), or if resolution fails/yields no
// addresses. It is the single source of truth shared by ValidateURLSafety,
// the SSRF-safe dialers and the redirect guard.
func resolveAndCheck(ctx context.Context, host string) ([]net.IP, error) {
	// ALWAYS derive a child context capped at ssrfLookupTimeout: every DNS
	// resolution has a hard 5s ceiling regardless of the caller's (possibly
	// much longer) deadline, while a SHORTER parent deadline still wins —
	// context.WithTimeout clamps to whichever ends first.
	ctx, cancel := context.WithTimeout(ctx, ssrfLookupTimeout)
	defer cancel()
	// Literal IPs skip the resolver (same semantics as DefaultResolver, one
	// fewer lookup on the hot path) and are policy-checked directly.
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return nil, &outboundErr{kind: "restricted", host: ip.String(), msg: fmt.Sprintf("ssrf: restricted address %s", ip)}
		}
		return []net.IP{ip}, nil
	}
	ips, err := lookupIPAddr(ctx, host)
	if err != nil {
		return nil, &outboundErr{kind: "unresolved", host: host, msg: fmt.Sprintf("ssrf: failed to resolve %q: %v", host, err)}
	}
	if len(ips) == 0 {
		return nil, &outboundErr{kind: "unresolved", host: host, msg: fmt.Sprintf("ssrf: %q resolved to no addresses", host)}
	}
	for _, addr := range ips {
		if blockedIP(addr.IP) {
			return nil, &outboundErr{kind: "restricted", host: addr.IP.String(), msg: fmt.Sprintf("ssrf: %q resolves to restricted address %s", host, addr.IP)}
		}
	}
	out := make([]net.IP, len(ips))
	for i, addr := range ips {
		out[i] = addr.IP
	}
	return out, nil
}

// ValidateURLSafetyDetail is the shared URL admission policy for
// admin-configured outbound targets (providers, MCP servers, gateway URLs):
// http/https scheme, host present, DNS resolution hard-capped at
// ssrfLookupTimeout, and every resolved address must pass the blockedIP
// policy above — exactly what the outbound dialers and the redirect guard
// enforce at connection time. On success it additionally returns the
// allowlist admission details (actual resolved IP + matching CIDR) derived
// from the SAME resolution that made the decision, so callers can audit
// without a second DNS lookup; Hit is non-nil only when the URL passed VIA
// the allowlist (private target). Public URLs pass with a nil Hit.
//
// Surrounding whitespace is trimmed before parsing: URLs pasted from chat
// windows or documents routinely carry an invisible trailing space or
// newline, and a raw parse of such a string fails as "invalid URL" — a
// failure class the entry checks used to misreport as an internal-address
// rejection. Trimming cannot change the target: whitespace inside a URL
// remains invalid and still fails the parse.
func ValidateURLSafetyDetail(rawURL string) (*AllowlistHit, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, &outboundErr{kind: "invalid", msg: fmt.Sprintf("invalid URL: %v", err)}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, &outboundErr{kind: "invalid", msg: "URL scheme must be http or https"}
	}
	host := u.Hostname()
	if host == "" {
		return nil, &outboundErr{kind: "invalid", msg: "URL host is empty"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), ssrfLookupTimeout)
	defer cancel()
	ips, err := resolveAndCheck(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if isHardDeniedIP(ip) {
			continue // unreachable: resolveAndCheck already rejected it
		}
		if isRestrictedIP(ip) {
			if p, ok := allowlistHitFor(ip); ok {
				return &AllowlistHit{IP: ip.String(), CIDR: p.String()}, nil
			}
		}
	}
	return nil, nil
}

// ValidateURLSafety is the boolean form of ValidateURLSafetyDetail for callers
// that do not need audit details.
func ValidateURLSafety(rawURL string) error {
	_, err := ValidateURLSafetyDetail(rawURL)
	return err
}

type ssrfSafeDialer struct {
	dialer *net.Dialer
}

// NewSSRFSafeDialer returns a DialContext function that validates resolved IPs
// against restricted ranges (loopback, private, link-local, multicast, unspecified)
// and uses a Control callback to prevent DNS rebinding attacks.
func NewSSRFSafeDialer(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &ssrfSafeDialer{dialer: &net.Dialer{Timeout: timeout}}
	return d.DialContext
}

func (d *ssrfSafeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("ssrf: invalid address %q: %w", addr, err)
	}
	ips, err := resolveAndCheck(ctx, host)
	if err != nil {
		return nil, err
	}
	// Use Control callback to validate the actual destination IP at socket level,
	// preventing DNS rebinding between resolution and connection.
	dd := *d.dialer
	dd.Control = ssrfControl
	conn, err := dd.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	if err == nil {
		auditAllowlistedDial(ips[0])
		// Track for immediate revocation on a later policy narrowing (see
		// conntrack.go): pool reuse must never bypass the active policy.
		conn = trackConn(conn, ips[0])
	}
	return conn, err
}

// ssrfTLSDialer is the TLS counterpart of ssrfSafeDialer. It resolves and checks
// the host, dials the resolved IP with a DNS-rebinding guard, then performs the
// TLS handshake against the original hostname (preserving SNI + cert verification).
type ssrfTLSDialer struct {
	dialer *net.Dialer
	base   *tls.Config
}

// NewSSRFSafeTLSDialer returns a DialTLSContext that applies the same SSRF
// protections as NewSSRFSafeDialer for https:// targets. The supplied base config
// (e.g. NextProtos to force HTTP/1.1 for SSE) is honored; ServerName is set from
// the dialed hostname when not already specified.
func NewSSRFSafeTLSDialer(timeout time.Duration, base *tls.Config) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &ssrfTLSDialer{dialer: &net.Dialer{Timeout: timeout}, base: base}
	return d.DialTLSContext
}

func (d *ssrfTLSDialer) DialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("ssrf: invalid address %q: %w", addr, err)
	}
	ips, err := resolveAndCheck(ctx, host)
	if err != nil {
		return nil, err
	}
	dd := *d.dialer
	dd.Control = ssrfControl
	raw, err := dd.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	if err != nil {
		return nil, err
	}
	auditAllowlistedDial(ips[0])
	// Track the RAW socket (closing it kills the TLS conn riding on it) for
	// immediate revocation on a later policy narrowing (see conntrack.go).
	raw = trackConn(raw, ips[0])
	cfg := tls.Config{}
	if d.base != nil {
		cfg = *d.base.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	tlsConn := tls.Client(raw, &cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("ssrf: tls handshake with %q: %w", host, err)
	}
	return tlsConn, nil
}
