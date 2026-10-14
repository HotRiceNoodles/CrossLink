package admin

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/crosslink/internal/guardrail"
)

// withAllowlist applies cidrs for the duration of one subtest and always
// restores strict mode afterwards, so allowlist tests cannot leak state into
// the rest of the admin suite.
func withAllowlist(t *testing.T, cidrs ...string) {
	t.Helper()
	if err := guardrail.SetInternalAllowlist(cidrs); err != nil {
		t.Fatalf("set allowlist %v: %v", cidrs, err)
	}
	t.Cleanup(func() { _ = guardrail.SetInternalAllowlist(nil) })
}

func TestEnsureOutboundURLAllowed_StrictDefault(t *testing.T) {
	denied := []string{
		"http://127.0.0.1/v1",           // loopback
		"http://[::1]/v1",               // loopback v6
		"http://[::ffff:127.0.0.1]/v1",  // mapped loopback
		"http://169.254.169.254/latest", // cloud metadata
		"http://[fe80::1]/v1",           // link-local v6
		"http://0.0.0.0/v1",             // unspecified
		"http://[ff02::1]/v1",           // multicast
		"http://10.1.2.3/v1",            // private, not allowlisted
		"http://192.168.1.10/v1",        // private, not allowlisted
		"http://[fd00::1]/v1",           // ULA, not allowlisted
	}
	for _, raw := range denied {
		if err := ensureOutboundURLAllowed(raw); err == nil {
			t.Errorf("ensureOutboundURLAllowed(%q) = nil, want rejection (strict default)", raw)
		}
	}
	if err := ensureOutboundURLAllowed(""); err != nil {
		t.Errorf("empty URL should short-circuit, got %v", err)
	}
	if err := ensureOutboundURLAllowed("ftp://example.com/v1"); err == nil {
		t.Error("non-http scheme must be rejected")
	}
}

func TestEnsureOutboundURLAllowed_AllowlistedPrivate(t *testing.T) {
	withAllowlist(t, "10.0.0.0/8", "192.168.0.0/16", "fd00:db8::/48")

	allowed := []string{
		"http://10.1.2.3/v1",
		"http://192.168.1.10/v1",
		"http://[fd00:db8::1]/v1",
		"https://8.8.8.8/", // public never needs the allowlist
	}
	for _, raw := range allowed {
		if err := ensureOutboundURLAllowed(raw); err != nil {
			t.Errorf("ensureOutboundURLAllowed(%q) = %v, want allowed (allowlisted)", raw, err)
		}
	}

	// The red line stays blocked even with wide private allowlists active —
	// including the non-link-local cloud metadata targets.
	stillDenied := []string{
		"http://127.0.0.1/v1",
		"http://[::1]/v1",
		"http://[::ffff:127.0.0.1]/v1",
		"http://169.254.169.254/latest",
		"http://[fe80::1]/v1",
		"http://[fd00:ec2::254]/latest", // AWS IPv6 IMDS lives inside ULA space
		"http://100.100.100.200/latest", // Alibaba metadata (CGNAT space)
		"http://172.16.0.9/v1",          // private but outside the allowlisted supernets
	}
	for _, raw := range stillDenied {
		if err := ensureOutboundURLAllowed(raw); err == nil {
			t.Errorf("ensureOutboundURLAllowed(%q) = nil, want rejection (red line / outside allowlist)", raw)
		}
	}
}

func TestEnsureOutboundURLAllowed_HostnameResolvesInternal(t *testing.T) {
	// "localhost" resolves to loopback on every resolver — the hard-denied
	// red line, with or without an allowlist.
	if err := ensureOutboundURLAllowed("http://localhost:11434"); err == nil {
		t.Error("localhost must stay blocked (loopback red line)")
	}
	withAllowlist(t, "192.168.0.0/16")
	if err := ensureOutboundURLAllowed("http://localhost:11434"); err == nil {
		t.Error("localhost must stay blocked even with a private allowlist active")
	}
}

// The admission audit is one Info record with a fixed category, the actual IP
// and the matching CIDR — and nothing else (no URL, path, credentials, query).
// Refusals and public targets never emit an admission record.
func TestEnsureOutboundURLAllowed_AuditRecord(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	// Denied internal target (strict default): no admission record.
	if err := ensureOutboundURLAllowed("http://10.99.0.7:11434/v1"); err == nil {
		t.Fatal("strict default should reject")
	}
	if strings.Contains(buf.String(), "admitted") {
		t.Errorf("refusal produced an admission record: %q", buf.String())
	}

	// Public target: passes, but no admission record (nothing was allowlisted).
	buf.Reset()
	if err := ensureOutboundURLAllowed("https://8.8.8.8/v1"); err != nil {
		t.Fatalf("public target rejected: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("public target produced a record: %q", buf.String())
	}

	// Allowlisted private target (with credentials/query in the URL): exactly
	// one Info admission record carrying ip + cidr, never the URL itself.
	buf.Reset()
	withAllowlist(t, "10.99.0.0/16")
	if err := ensureOutboundURLAllowed("http://user:secret@10.99.0.7:11434/v1?token=x"); err != nil {
		t.Fatalf("allowlisted target rejected: %v", err)
	}
	out := buf.String()
	if n := strings.Count(out, "outbound URL admitted"); n != 1 {
		t.Fatalf("admission records = %d, want exactly 1: %q", n, out)
	}
	for _, want := range []string{"ip=10.99.0.7", "cidr=10.99.0.0/16"} {
		if !strings.Contains(out, want) {
			t.Errorf("admission record missing %s: %q", want, out)
		}
	}
	for _, leak := range []string{"user", "secret", "token", "/v1", "11434"} {
		if strings.Contains(out, leak) {
			t.Errorf("admission record leaks %q: %q", leak, out)
		}
	}

	// Red-line refusal under a wide allowlist: still no admission record.
	buf.Reset()
	withAllowlist(t, "10.0.0.0/8", "192.168.0.0/16")
	if err := ensureOutboundURLAllowed("http://169.254.169.254/latest"); err == nil {
		t.Fatal("red line must refuse")
	}
	if strings.Contains(buf.String(), "admitted") {
		t.Errorf("red-line refusal produced an admission record: %q", buf.String())
	}
}
