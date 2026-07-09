package guardrail

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIsRestrictedIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"private 10.x", "10.0.0.1", true},
		{"private 172.16.x", "172.16.0.1", true},
		{"private 192.168.x", "192.168.1.1", true},
		{"link-local", "169.254.0.1", true},
		{"unspecified", "0.0.0.0", true},
		{"public 8.8.8.8", "8.8.8.8", false},
		{"public 1.1.1.1", "1.1.1.1", false},
		{"multicast", "224.0.0.1", true},
		{"ipv6 private fd00", "fd00::1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse IP %q", tt.ip)
			}
			got := isRestrictedIP(ip)
			if got != tt.want {
				t.Errorf("isRestrictedIP(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

// TestSSRFSafeDialer_BlocksInternal asserts the dialer refuses to even attempt a
// connection to a loopback address — the SSRF check fails before any dial.
func TestSSRFSafeDialer_BlocksInternal(t *testing.T) {
	dial := NewSSRFSafeDialer(2 * time.Second)
	_, err := dial(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatal("expected dial to 127.0.0.1 to be blocked, got nil error")
	}
	if !strings.Contains(err.Error(), "restricted") {
		t.Fatalf("expected restricted-address error, got %v", err)
	}
}

// TestInternalAllowlist verifies the operator CIDR allowlist permits allowlisted
// restricted ranges while keeping everything else (loopback, link-local/cloud
// metadata, other private ranges) blocked. Restores the strict default after.
func TestInternalAllowlist(t *testing.T) {
	// Strict default: all restricted blocked.
	if AllowedInternal(net.ParseIP("10.0.0.1")) {
		t.Fatal("10.0.0.1 should not be allowed with empty allowlist")
	}
	if err := SetInternalAllowlist([]string{"10.0.0.0/8", " 192.168.0.0/16 "}); err != nil {
		t.Fatalf("SetInternalAllowlist: %v", err)
	}
	t.Cleanup(func() { _ = SetInternalAllowlist(nil) })

	cases := []struct {
		ip      string
		allowed bool
	}{
		{"10.0.0.5", true},      // allowlisted VPC range
		{"192.168.1.1", true},   // allowlisted
		{"127.0.0.1", false},    // loopback never listed → blocked
		{"169.254.169.254", false}, // cloud metadata → blocked
		{"172.16.0.1", false},   // private but not listed → blocked
		{"8.8.8.8", false},      // public — not routed through AllowedInternal
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if got := AllowedInternal(ip); got != c.allowed {
			t.Errorf("AllowedInternal(%s) = %v, want %v", c.ip, got, c.allowed)
		}
		// blockedIP must agree for restricted addresses: blocked = restricted && !allowed.
		if got := blockedIP(ip); got != (!c.allowed && isRestrictedIP(ip)) {
			t.Errorf("blockedIP(%s) = %v, want %v", c.ip, got, !c.allowed && isRestrictedIP(ip))
		}
	}

	// Invalid CIDR → error, allowlist unchanged (10.x still allowed).
	if err := SetInternalAllowlist([]string{"not-a-cidr"}); err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
	if !AllowedInternal(net.ParseIP("10.0.0.1")) {
		t.Fatal("allowlist should be unchanged after invalid input")
	}
}

// TestSSRFSafeDialer_AllowlistPasses confirms an allowlisted internal address
// clears the SSRF check (it then fails at the actual connect, not with
// "restricted").
func TestSSRFSafeDialer_AllowlistPasses(t *testing.T) {
	if err := SetInternalAllowlist([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("SetInternalAllowlist: %v", err)
	}
	t.Cleanup(func() { _ = SetInternalAllowlist(nil) })

	dial := NewSSRFSafeDialer(500 * time.Millisecond)
	_, err := dial(context.Background(), "tcp", "10.255.255.1:80") // non-routable → connect timeout/refused
	if err == nil {
		t.Fatal("expected a connect error (no listener), not nil")
	}
	if strings.Contains(err.Error(), "restricted") {
		t.Fatalf("allowlisted 10.x should not be blocked as restricted, got %v", err)
	}
}

// TestSSRFSafeTLSDialer_BlocksInternal is the TLS counterpart.
func TestSSRFSafeTLSDialer_BlocksInternal(t *testing.T) {
	dial := NewSSRFSafeTLSDialer(2*time.Second, nil)
	_, err := dial(context.Background(), "tcp", "127.0.0.1:443")
	if err == nil {
		t.Fatal("expected TLS dial to 127.0.0.1 to be blocked, got nil error")
	}
	if !strings.Contains(err.Error(), "restricted") {
		t.Fatalf("expected restricted-address error, got %v", err)
	}
}

// TestBlockInternalRedirect verifies the CheckRedirect policy refuses redirects
// to restricted addresses and caps the redirect chain.
func TestBlockInternalRedirect(t *testing.T) {
	// Redirect to loopback IP literal → blocked (no network needed: IP literals
	// are resolved locally by LookupIPAddr).
	loopbackReq := &http.Request{URL: mustParse(t, "http://127.0.0.1/latest/meta-data/")}
	if err := BlockInternalRedirect(loopbackReq, nil); err == nil {
		t.Fatal("expected redirect to 127.0.0.1 to be blocked")
	}

	// Redirect to a public IP literal → allowed.
	publicReq := &http.Request{URL: mustParse(t, "http://8.8.8.8/")}
	if err := BlockInternalRedirect(publicReq, nil); err != nil {
		t.Fatalf("expected redirect to 8.8.8.8 to be allowed, got %v", err)
	}

	// Too many redirects → blocked regardless of target.
	via := make([]*http.Request, 10)
	if err := BlockInternalRedirect(publicReq, via); err == nil {
		t.Fatal("expected redirect chain cap to trigger")
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
