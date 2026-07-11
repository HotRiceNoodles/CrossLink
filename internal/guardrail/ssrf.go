package guardrail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const ssrfLookupTimeout = 5 * time.Second

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

// SetInternalAllowlist configures the restricted CIDRs outbound SSRF-safe
// connections may reach, in addition to all public addresses. Pass an empty
// slice to (re)apply the strict default. An invalid CIDR returns an error and
// leaves the active allowlist unchanged.
func SetInternalAllowlist(cidrs []string) error {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return fmt.Errorf("ssrf: invalid allowlist CIDR %q: %w", c, err)
		}
		prefixes = append(prefixes, p)
	}
	if len(prefixes) == 0 {
		internalAllowlist.Store(nil)
	} else {
		internalAllowlist.Store(&internalCIDRSet{prefixes: prefixes})
	}
	return nil
}

// AllowedInternal reports whether ip is an internal/restricted address that the
// operator has explicitly allowlisted. Public addresses are never routed here.
func AllowedInternal(ip net.IP) bool {
	set := internalAllowlist.Load()
	if set == nil {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range set.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// blockedIP is the connection policy: an IP is blocked only if it is restricted
// AND not operator-allowlisted. This is what dialers and the redirect guard use.
func blockedIP(ip net.IP) bool {
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

// resolveAndCheck resolves host and returns an error if any resolved address is
// in a restricted range, or if resolution fails/yields no addresses. It is the
// single source of truth shared by ValidateURLSafety, the SSRF-safe dialers and
// the redirect guard.
func resolveAndCheck(ctx context.Context, host string) ([]net.IP, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("ssrf: failed to resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("ssrf: %q resolved to no addresses", host)
	}
	for _, addr := range ips {
		if blockedIP(addr.IP) {
			return nil, fmt.Errorf("ssrf: %q resolves to restricted address %s", host, addr.IP)
		}
	}
	out := make([]net.IP, len(ips))
	for i, addr := range ips {
		out[i] = addr.IP
	}
	return out, nil
}

func ValidateURLSafety(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL host is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), ssrfLookupTimeout)
	defer cancel()
	if _, err := resolveAndCheck(ctx, host); err != nil {
		return err
	}
	return nil
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
	return dd.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
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
