package guardrail

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// restoreStrict reapplies the strict default after every test that touches
// the global allowlist, so tests can never leak a widened policy into the
// rest of the package's suites.
func restoreStrict(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = SetInternalAllowlist(nil) })
}

func mustIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("parse IP %q", s)
	}
	return ip
}

// --- SetInternalAllowlist: only private subnets are allowlistable ---

func TestSetInternalAllowlist_OnlyPrivateSubnets(t *testing.T) {
	restoreStrict(t)
	valid := []string{
		"10.0.0.0/8", "10.1.2.0/24", // exact supernet and subnet
		"172.16.0.0/12", "172.20.3.0/28", //
		"192.168.0.0/16", "192.168.65.0/24", // the Docker Desktop host range
		"fd00:db8::/64", "fd12::/48", // IPv6 ULA outside the AWS block
	}
	if err := SetInternalAllowlist(valid); err != nil {
		t.Fatalf("valid private CIDRs rejected: %v", err)
	}
	// spot-check the loaded set actually permits
	if !AllowedInternal(mustIP(t, "192.168.65.5")) {
		t.Error("192.168.65.5 should be allowlisted by 192.168.65.0/24")
	}
	if !AllowedInternal(mustIP(t, "172.16.0.1")) {
		t.Error("172.16.0.1 should be allowlisted by 172.16.0.0/12")
	}
	if !AllowedInternal(mustIP(t, "fd00:db8::dead:beef")) {
		t.Error("fd00:db8::dead:beef should be allowlisted by fd00:db8::/64")
	}
	// Outside-the-allowlist negatives are covered by TestBlockedIP_PrivateSoftRestricted.

	invalid := []struct {
		cidr string
		why  string
	}{
		{"127.0.0.0/8", "loopback"},
		{"::1/128", "loopback v6"},
		{"169.254.0.0/16", "link-local / cloud metadata"},
		{"fe80::/10", "link-local v6"},
		{"0.0.0.0/0", "unspecified / everything"},
		{"::/0", "everything v6"},
		{"224.0.0.0/4", "multicast"},
		{"ff00::/8", "multicast v6"},
		{"8.8.8.0/24", "public space"},
		{"2001:db8::/32", "public v6"},
		{"10.0.0.0/7", "wider than the private supernet"},
		{"::ffff:10.0.0.0/104", "IPv4-mapped IPv6 prefix"},
		{"not-a-cidr", "syntax"},
		// Cloud-metadata overlap (AWS IPv6 IMDS lives at fd00:ec2::254 INSIDE
		// the ULA range — wide and exact entries alike must be refused):
		{"fc00::/7", "whole ULA supernet overlaps the AWS infrastructure block"},
		{"fd00::/8", "overlaps the AWS infrastructure block"},
		{"fd00:ec2::254/128", "exact AWS IPv6 IMDS address"},
		{"fd00:ec2::/64", "subnet inside the AWS infrastructure block"},
		{"100.100.100.200/32", "Alibaba metadata endpoint (also not a private subnet)"},
	}
	for _, tt := range invalid {
		if err := SetInternalAllowlist([]string{tt.cidr}); err == nil {
			t.Errorf("SetInternalAllowlist(%q) accepted, want rejection (%s)", tt.cidr, tt.why)
		}
	}
}

func TestSetInternalAllowlist_AtomicAllOrNothing(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("seed allowlist: %v", err)
	}
	// A list with one bad entry must be rejected AND leave the previous
	// allowlist fully in effect — no partial application.
	err := SetInternalAllowlist([]string{"192.168.0.0/16", "127.0.0.0/8", "10.9.9.0/24"})
	if err == nil {
		t.Fatal("mixed list accepted, want rejection")
	}
	if !AllowedInternal(mustIP(t, "10.0.0.1")) {
		t.Error("previous allowlist lost after rejected update — not atomic")
	}
	if AllowedInternal(mustIP(t, "192.168.0.1")) {
		t.Error("rejected entry took effect — partial application")
	}
}

// --- blockedIP: red line overrides any allowlist content ---

func TestBlockedIP_HardDenyRedLine(t *testing.T) {
	restoreStrict(t)
	// Defense in depth: even a (hypothetical, hand-crafted) allowlist entry
	// covering red-line space can never permit it, because blockedIP checks
	// the red line FIRST. SetInternalAllowlist already refuses such entries;
	// this bypasses the setter to pin the runtime invariant.
	internalAllowlist.Store(&internalCIDRSet{prefixes: mustPrefixes(t,
		"0.0.0.0/0", "::/0", "127.0.0.0/8", "169.254.0.0/16")})

	redLine := []string{
		"127.0.0.1", "::1", "::ffff:127.0.0.1", // loopback incl. mapped
		"169.254.169.254", "fe80::1", "169.254.0.9", // link-local / metadata
		"fd00:ec2::254", "fd00:ec2:1234:5678::1", // AWS IPv6 IMDS + reserved block
		"100.100.100.200", // Alibaba metadata endpoint (CGNAT space)
		"0.0.0.0", "::",   // unspecified
		"224.0.0.1", "ff02::1", "239.1.1.1", // multicast
	}
	for _, s := range redLine {
		if !blockedIP(mustIP(t, s)) {
			t.Errorf("blockedIP(%s) = false with an everything-allowlist active; red line must be absolute", s)
		}
	}
	// Private targets still honor the (crafted) allowlist in this fixture;
	// adjacent non-metadata CGNAT space is not collateral-blocked.
	if blockedIP(mustIP(t, "10.0.0.1")) {
		t.Error("blockedIP(10.0.0.1) = true under crafted 0.0.0.0/0 allowlist")
	}
	if blockedIP(mustIP(t, "fd00:db8::1")) {
		t.Error("blockedIP(fd00:db8::1) = true under crafted ::/0 allowlist")
	}
	if blockedIP(mustIP(t, "100.100.100.201")) {
		t.Error("blockedIP(100.100.100.201) = true; only the exact metadata /32 is denied")
	}
}

func TestBlockedIP_PrivateSoftRestricted(t *testing.T) {
	restoreStrict(t)
	// Strict default: all private blocked.
	for _, s := range []string{"10.0.0.1", "172.17.0.2", "192.168.1.1", "fd00:db8::1", "::ffff:10.2.3.4"} {
		if !blockedIP(mustIP(t, s)) {
			t.Errorf("blockedIP(%s) = false under strict default", s)
		}
	}
	if err := SetInternalAllowlist([]string{"10.0.0.0/8", "fd00:db8::/48"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	for _, s := range []string{"10.0.0.1", "10.255.0.1", "fd00:db8::1", "::ffff:10.2.3.4"} {
		if blockedIP(mustIP(t, s)) {
			t.Errorf("blockedIP(%s) = true while allowlisted", s)
		}
	}
	for _, s := range []string{"172.17.0.2", "192.168.1.1"} {
		if !blockedIP(mustIP(t, s)) {
			t.Errorf("blockedIP(%s) = false outside the allowlist", s)
		}
	}
}

func mustPrefixes(t *testing.T, cidrs ...string) (out []netip.Prefix) {
	t.Helper()
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("parse %q: %v", c, err)
		}
		out = append(out, p)
	}
	return out
}

// --- resolveAndCheck via the DNS injection seam ---

func withResolver(t *testing.T, fn func(ctx context.Context, host string) ([]net.IPAddr, error)) {
	t.Helper()
	prev := lookupIPAddr
	lookupIPAddr = fn
	t.Cleanup(func() { lookupIPAddr = prev })
}

func addrs(ips ...string) []net.IPAddr {
	out := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out
}

func TestResolveAndCheck_MixedRecordsConservative(t *testing.T) {
	restoreStrict(t)
	withResolver(t, func(_ context.Context, host string) ([]net.IPAddr, error) {
		return addrs("8.8.8.8", "10.0.0.9"), nil
	})
	// Without an allowlist: one restricted record among many -> reject.
	if _, err := resolveAndCheck(context.Background(), "svc.internal"); err == nil {
		t.Error("mixed public+private records must be rejected without an allowlist")
	}
	// With 10/8 allowlisted: the same mixed set passes.
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	if _, err := resolveAndCheck(context.Background(), "svc.internal"); err != nil {
		t.Errorf("mixed records with all-private-resolved-and-allowlisted rejected: %v", err)
	}
	// A red-line record in the mix rejects regardless of allowlist.
	withResolver(t, func(_ context.Context, host string) ([]net.IPAddr, error) {
		return addrs("8.8.8.8", "169.254.169.254"), nil
	})
	if _, err := resolveAndCheck(context.Background(), "evil.internal"); err == nil {
		t.Error("metadata record in the mix must be rejected even with a private allowlist")
	}
}

func TestResolveAndCheck_FailureAndEmptyFailClosed(t *testing.T) {
	restoreStrict(t)
	withResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return nil, fmt.Errorf("dns down")
	})
	if _, err := resolveAndCheck(context.Background(), "x.test"); err == nil {
		t.Error("resolver error must fail closed")
	}
	withResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return nil, nil
	})
	if _, err := resolveAndCheck(context.Background(), "x.test"); err == nil {
		t.Error("empty answer must fail closed")
	}
}

func TestResolveAndCheck_BoundedTimeout(t *testing.T) {
	restoreStrict(t)
	prevTimeout := ssrfLookupTimeout
	ssrfLookupTimeout = 150 * time.Millisecond
	t.Cleanup(func() { ssrfLookupTimeout = prevTimeout })

	hang := func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		<-ctx.Done() // simulate an unresponsive resolver
		return nil, ctx.Err()
	}

	// No parent deadline: the 5s (here 150ms) cap applies.
	withResolver(t, hang)
	start := time.Now()
	if _, err := resolveAndCheck(context.Background(), "slow.test"); err == nil {
		t.Fatal("hanging resolver must produce an error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("lookup not bounded by ssrfLookupTimeout: took %s", elapsed)
	}

	// LONG parent deadline (minutes): the cap must still apply — the derived
	// child context ends at min(parent, cap).
	withResolver(t, hang)
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start = time.Now()
	if _, err := resolveAndCheck(parent, "slow-long-deadline.test"); err == nil {
		t.Fatal("hanging resolver with a long parent deadline must produce an error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("long parent deadline defeated the 5s cap: took %s", elapsed)
	}

	// SHORTER parent deadline wins over the cap: the child context ends at
	// the parent's earlier deadline.
	withResolver(t, hang)
	parentShort, cancelShort := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancelShort()
	start = time.Now()
	if _, err := resolveAndCheck(parentShort, "slow-short-deadline.test"); err == nil {
		t.Fatal("hanging resolver with a short parent deadline must produce an error")
	}
	if elapsed := time.Since(start); elapsed >= 150*time.Millisecond {
		t.Fatalf("short parent deadline did not win over the cap: took %s", elapsed)
	}
}

// --- ValidateURLSafetyDetail: audit details without a second resolution ---

func TestValidateURLSafetyDetail_DomainTargetAndDNSChange(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}

	current := "10.0.0.1"
	withResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return addrs(current), nil
	})

	// Domain-named private target: hit carries the ACTUAL resolved IP and the
	// matching CIDR — the audit trail no longer misses domain targets.
	hit, err := ValidateURLSafetyDetail("http://llm.internal:8000/v1")
	if err != nil {
		t.Fatalf("domain private target rejected: %v", err)
	}
	if hit == nil || hit.IP != "10.0.0.1" || hit.CIDR != "10.0.0.0/8" {
		t.Fatalf("hit = %+v, want {10.0.0.1 10.0.0.0/8}", hit)
	}

	// DNS change: the next decision reflects the new resolution — the audited
	// IP is always the one the decision was actually made on.
	current = "10.200.3.4"
	hit, err = ValidateURLSafetyDetail("http://llm.internal:8000/v1")
	if err != nil {
		t.Fatalf("changed-resolution private target rejected: %v", err)
	}
	if hit == nil || hit.IP != "10.200.3.4" {
		t.Fatalf("hit after DNS change = %+v, want IP 10.200.3.4", hit)
	}

	// Resolution flips public: passes, but no allowlist hit to audit.
	current = "8.8.8.8"
	hit, err = ValidateURLSafetyDetail("http://llm.internal:8000/v1")
	if err != nil {
		t.Fatalf("public resolution rejected: %v", err)
	}
	if hit != nil {
		t.Fatalf("public resolution produced an allowlist hit: %+v", hit)
	}

	// Resolution flips to a red-line address: refused, no hit.
	current = "169.254.169.254"
	hit, err = ValidateURLSafetyDetail("http://llm.internal:8000/v1")
	if err == nil || hit != nil {
		t.Fatalf("metadata resolution must be refused with no hit: err=%v hit=%+v", err, hit)
	}
}

// --- DNS rebinding: admin check and dial must not share resolution state ---

func TestDialer_RebindingBetweenCheckAndDial(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	calls := 0
	withResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		calls++
		if calls == 1 {
			return addrs("10.0.0.5"), nil // admission check sees an allowed IP
		}
		// The DIAL resolves again — a rebinding attacker now answers loopback.
		return addrs("127.0.0.1"), nil
	})
	// Admission passes...
	if err := ValidateURLSafety("http://rebind.test/v1"); err != nil {
		t.Fatalf("admission unexpectedly rejected: %v", err)
	}
	// ...but the dialer re-validates independently and refuses the rebound IP.
	dial := NewSSRFSafeDialer(2 * time.Second)
	if _, err := dial(context.Background(), "tcp", "rebind.test:80"); err == nil {
		t.Fatal("dialer accepted a rebound loopback address — rebinding guard broken")
	}
}

// --- ValidateURLSafety end-to-end with injected DNS ---

func TestValidateURLSafety_Matrix(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.0.0.0/8", "fd00:db8::/48"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	withResolver(t, func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "ok-private.test":
			return addrs("10.1.2.3"), nil
		case "hard-denied.test":
			return addrs("8.8.8.8", "127.0.0.2"), nil
		case "public.test":
			return addrs("1.2.3.4"), nil
		default:
			return addrs("8.8.8.8"), nil
		}
	})
	if err := ValidateURLSafety("http://ok-private.test/v1"); err != nil {
		t.Errorf("allowlisted private target rejected: %v", err)
	}
	if err := ValidateURLSafety("https://public.test"); err != nil {
		t.Errorf("public target rejected: %v", err)
	}
	if err := ValidateURLSafety("http://hard-denied.test/v1"); err == nil {
		t.Error("loopback in resolution set must be rejected despite allowlist")
	}
	for _, raw := range []string{"ftp://public.test", "http://", "://missing-scheme"} {
		if err := ValidateURLSafety(raw); err == nil {
			t.Errorf("ValidateURLSafety(%q) accepted malformed/scheme-less URL", raw)
		}
	}
}

// --- Redirect guard shares the same policy ---

func TestBlockInternalRedirect_Policy(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	withResolver(t, func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "allowed-private.test":
			return addrs("10.3.2.1"), nil
		default:
			return addrs("8.8.8.8"), nil
		}
	})
	mkReq := func(raw string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		return req
	}
	if err := BlockInternalRedirect(mkReq("http://allowed-private.test/x"), nil); err != nil {
		t.Errorf("redirect to allowlisted private target refused: %v", err)
	}
	if err := BlockInternalRedirect(mkReq("http://169.254.169.254/latest"), nil); err == nil {
		t.Error("redirect to cloud metadata must be refused")
	}
	if err := BlockInternalRedirect(mkReq("http://127.0.0.1/x"), nil); err == nil {
		t.Error("redirect to loopback must be refused")
	}
	// A hostname resolving to loopback is refused too (via the injected resolver).
	withResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return addrs("::1"), nil
	})
	if err := BlockInternalRedirect(mkReq("http://loopback-name.test/x"), nil); err == nil {
		t.Error("redirect to loopback-resolving name must be refused")
	}
}

// --- ssrfControl: final socket IP check ---

func TestSSRFControl_FinalSocketIP(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	if err := ssrfControl("tcp", "10.0.0.7:443", nil); err != nil {
		t.Errorf("ssrfControl(allowlisted) = %v, want nil", err)
	}
	for _, addr := range []string{
		"127.0.0.1:443", "[::1]:443", "[::ffff:127.0.0.1]:443",
		"169.254.169.254:80", "0.0.0.0:80", "224.0.0.1:80",
	} {
		if err := ssrfControl("tcp", addr, nil); err == nil {
			t.Errorf("ssrfControl(%s) = nil, want rejection", addr)
		}
	}
	if err := ssrfControl("tcp", "192.168.1.1:443", nil); err == nil {
		t.Error("ssrfControl(private outside allowlist) = nil, want rejection")
	}
	if err := ssrfControl("tcp", "not-an-ip:443", nil); err == nil {
		t.Error("ssrfControl(non-IP) = nil, want rejection")
	}
}

// --- cloud-metadata targets that do NOT live in link-local space ---
// AWS IPv6 IMDS answers at fd00:ec2::254 INSIDE the allowlistable ULA range;
// Alibaba metadata sits at 100.100.100.200 in CGNAT space. Both must be
// unconditionally denied at every layer, and any allowlist entry overlapping
// the AWS infrastructure block must be refused at configuration time.
// All addresses below are synthetic test targets — no real metadata endpoint
// is contacted.
func TestCloudMetadata_HardDenyAcrossLayers(t *testing.T) {
	restoreStrict(t)

	t.Run("wide and exact allowlist entries overlapping the blocks are refused", func(t *testing.T) {
		for _, cidr := range []string{"fc00::/7", "fd00::/8", "fd00:ec2::/64", "fd00:ec2::254/128"} {
			if err := SetInternalAllowlist([]string{cidr}); err == nil {
				t.Errorf("SetInternalAllowlist(%q) accepted; metadata-overlapping entries must be refused", cidr)
			}
		}
	})

	// With a VALID ULA allowlist active (outside the AWS block), every layer
	// still denies the metadata targets.
	if err := SetInternalAllowlist([]string{"fd00:db8::/48", "10.0.0.0/8"}); err != nil {
		t.Fatalf("valid ULA allowlist rejected: %v", err)
	}

	t.Run("admission (ValidateURLSafetyDetail)", func(t *testing.T) {
		for _, raw := range []string{"http://[fd00:ec2::254]/latest", "http://100.100.100.200/latest"} {
			hit, err := ValidateURLSafetyDetail(raw)
			if err == nil || hit != nil {
				t.Errorf("ValidateURLSafetyDetail(%q) = hit=%+v err=%v, want refusal with no hit", raw, hit, err)
			}
		}
		// Adjacent CGNAT address is NOT collateral-blocked (not metadata).
		if hit, err := ValidateURLSafetyDetail("http://100.100.100.201/v1"); err != nil || hit != nil {
			t.Errorf("adjacent non-metadata address mishandled: hit=%+v err=%v", hit, err)
		}
	})

	t.Run("final socket check (ssrfControl)", func(t *testing.T) {
		for _, addr := range []string{"[fd00:ec2::254]:80", "[fd00:ec2:1234:5678::1]:443", "100.100.100.200:80"} {
			if err := ssrfControl("tcp", addr, nil); err == nil {
				t.Errorf("ssrfControl(%s) = nil, want rejection (cloud metadata)", addr)
			}
		}
		if err := ssrfControl("tcp", "100.100.100.201:80", nil); err != nil {
			t.Errorf("ssrfControl(100.100.100.201) = %v; adjacent address must not be collateral-blocked", err)
		}
	})

	t.Run("redirect guard refuses metadata destination", func(t *testing.T) {
		withResolver(t, func(_ context.Context, host string) ([]net.IPAddr, error) {
			switch host {
			case "imds6.attacker.test":
				return addrs("fd00:ec2::254"), nil
			case "imds-ali.attacker.test":
				return addrs("100.100.100.200"), nil
			default:
				return addrs("8.8.8.8"), nil
			}
		})
		for _, raw := range []string{
			"http://imds6.attacker.test/latest",
			"http://imds-ali.attacker.test/latest",
		} {
			req := httptest.NewRequest(http.MethodGet, raw, nil)
			if err := BlockInternalRedirect(req, nil); err == nil {
				t.Errorf("redirect to %q accepted; metadata destination must be refused", raw)
			}
		}
	})

	t.Run("no allowlist admission record for metadata refusals", func(t *testing.T) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
		defer slog.SetDefault(prev)
		_, _ = ValidateURLSafetyDetail("http://[fd00:ec2::254]/latest")
		auditAllowlistedDial(mustIP(t, "fd00:ec2::254"))
		auditAllowlistedDial(mustIP(t, "100.100.100.200"))
		if strings.Contains(buf.String(), "admitted") {
			t.Errorf("metadata refusal produced an admission record: %q", buf.String())
		}
	})
}

// --- audit log: fixed category, IP + CIDR, default-visible level, no secrets ---

func TestAuditAllowlistedDial_NoSecrets(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	// Info handler = the DEFAULT log level: the record must be visible there.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	auditAllowlistedDial(mustIP(t, "10.4.5.6"))
	out := buf.String()
	if !strings.Contains(out, "ssrf allowlist: connection admitted") {
		t.Errorf("fixed-category Info record missing: %q", out)
	}
	for _, want := range []string{"ip=10.4.5.6", "cidr=10.0.0.0/8"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit record missing %s: %q", want, out)
		}
	}
	for _, secret := range []string{"http", "://", "user:", "password", "token", "body"} {
		if strings.Contains(out, secret) {
			t.Errorf("audit record leaks %q: %q", secret, out)
		}
	}
	// Red-line and public IPs produce no admission record.
	buf.Reset()
	auditAllowlistedDial(mustIP(t, "127.0.0.1"))
	auditAllowlistedDial(mustIP(t, "8.8.8.8"))
	if buf.Len() != 0 {
		t.Errorf("unexpected audit records for non-allowlisted dials: %q", buf.String())
	}
}

// --- surrounding whitespace: admission decides on the trimmed target ---

func TestValidateURLSafetyDetail_SurroundingWhitespace(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.99.0.0/16"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	// Pasted URLs routinely carry an invisible trailing space or newline (chat
	// windows, documents). The trimmed target is what the operator meant and
	// what every later dial would actually reach, so admission must decide on
	// it — not fail the raw parse and let callers misreport a copy artifact as
	// an internal-address rejection.
	for _, raw := range []string{
		"http://10.99.0.7:11434/v1 ",
		"http://10.99.0.7:11434/v1\n",
		"http://10.99.0.7:11434/v1\r\n",
		" http://10.99.0.7:11434/v1",
		"\thttp://10.99.0.7:11434/v1\t",
		"http://10.99.0.7:11434/v1　", // U+3000 full-width space
	} {
		hit, err := ValidateURLSafetyDetail(raw)
		if err != nil {
			t.Errorf("%q rejected: %v", raw, err)
			continue
		}
		if hit == nil || hit.IP != "10.99.0.7" || hit.CIDR != "10.99.0.0/16" {
			t.Errorf("%q hit = %+v, want {10.99.0.7 10.99.0.0/16}", raw, hit)
		}
	}
	// Whitespace-wrapped PUBLIC target: admitted, no allowlist hit.
	withResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return addrs("8.8.8.8"), nil
	})
	if hit, err := ValidateURLSafetyDetail(" https://api.example.com/v1\n"); err != nil || hit != nil {
		t.Errorf("public+whitespace: hit=%v err=%v", hit, err)
	}
	// Strict default: trimming does not widen anything — the wrapped
	// restricted target is still rejected exactly like the clean form.
	_ = SetInternalAllowlist(nil)
	if _, err := ValidateURLSafetyDetail("http://10.99.0.7:11434/v1 "); err == nil {
		t.Error("strict default admitted whitespace-wrapped restricted target")
	}
}

func TestValidateURLSafetyDetail_EmbeddedWhitespaceStaysInvalid(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	// Trimming must not mask a genuinely malformed target: whitespace INSIDE
	// the host is not a copy artifact and must fail (here: resolve failure,
	// deterministically injected).
	withResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return nil, fmt.Errorf("no such host")
	})
	if _, err := ValidateURLSafetyDetail("http://10.0 .0.7:11434"); err == nil {
		t.Error("embedded-space host admitted")
	}
}

// --- OutboundErrorInfo: classify admission failures for truthful messages ---

func TestOutboundErrorInfo_Classification(t *testing.T) {
	restoreStrict(t)
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("allowlist: %v", err)
	}

	// Restricted literal target outside the allowlist.
	_, err := ValidateURLSafetyDetail("http://192.168.1.10:8080")
	kind, host, ok := OutboundErrorInfo(err)
	if !ok || kind != "restricted" || host != "192.168.1.10" {
		t.Errorf("restricted: kind=%q host=%q ok=%v err=%v", kind, host, ok, err)
	}
	if err.Error() != "ssrf: restricted address 192.168.1.10" {
		t.Errorf("restricted wire message changed: %q", err.Error())
	}

	// Unresolvable host (injected resolver failure).
	withResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return nil, fmt.Errorf("no such host")
	})
	_, err = ValidateURLSafetyDetail("http://llm.onprem.example:8000")
	kind, host, ok = OutboundErrorInfo(err)
	if !ok || kind != "unresolved" || host != "llm.onprem.example" {
		t.Errorf("unresolved: kind=%q host=%q ok=%v err=%v", kind, host, ok, err)
	}

	// Domain resolving INTO a restricted range (rebinding shape).
	withResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return addrs("192.168.1.10"), nil
	})
	_, err = ValidateURLSafetyDetail("http://llm.onprem.example:8000")
	kind, host, ok = OutboundErrorInfo(err)
	if !ok || kind != "restricted" || host != "192.168.1.10" {
		t.Errorf("restricted-via-DNS: kind=%q host=%q ok=%v err=%v", kind, host, ok, err)
	}

	// Malformed URL (double colon — a genuine url.Parse failure; note Go does
	// NOT reject out-of-range port numbers syntactically, those flow into the
	// restricted-target path above).
	_, err = ValidateURLSafetyDetail("http://192.168.1.10::11434")
	kind, _, ok = OutboundErrorInfo(err)
	if !ok || kind != "invalid" {
		t.Errorf("invalid: kind=%q ok=%v err=%v", kind, ok, err)
	}
	if !strings.HasPrefix(err.Error(), "invalid URL: ") {
		t.Errorf("invalid wire message changed: %q", err.Error())
	}

	// Non-admission errors are not classified.
	_, _, ok = OutboundErrorInfo(context.Canceled)
	if ok {
		t.Error("unrelated error classified as outbound error")
	}
}
