package mcp

import (
	"testing"

	"github.com/crosslink/internal/guardrail"
)

// MCP/admin allowlist consistency: the MCP entry delegates to the same shared
// SSRF validator as the provider entries, so with a private allowlist active
// its verdict must match — private targets admitted, red line still closed.
func TestValidateServerURL_AllowlistConsistency(t *testing.T) {
	if err := guardrail.SetInternalAllowlist([]string{"10.99.0.0/16"}); err != nil {
		t.Fatalf("set allowlist: %v", err)
	}
	t.Cleanup(func() { _ = guardrail.SetInternalAllowlist(nil) })

	// Private target inside the allowlist: admitted (same as provider entries).
	if err := validateServerURL("http://10.99.0.7:11434/mcp"); err != nil {
		t.Errorf("allowlisted private MCP URL rejected: %v", err)
	}
	// Private target outside the allowlist: still rejected.
	if err := validateServerURL("http://192.168.1.4/mcp"); err == nil {
		t.Error("private MCP URL outside allowlist accepted")
	}
	// Red line: identical verdicts regardless of allowlist — including the
	// non-link-local cloud metadata targets inside ULA/CGNAT space.
	for _, raw := range []string{
		"http://127.0.0.1/mcp",
		"http://localhost/mcp",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/mcp",
		"http://[fd00:ec2::254]/latest", // AWS IPv6 IMDS
		"http://100.100.100.200/latest", // Alibaba metadata
	} {
		if err := validateServerURL(raw); err == nil {
			t.Errorf("validateServerURL(%q) accepted under allowlist; red line must stay closed", raw)
		}
	}
}
