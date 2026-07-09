package mcp

import (
	"strings"
	"testing"

	"github.com/crosslink/internal/config"
)

// TestCreateServer_StdioDisabledByDefault verifies the stdio MCP transport is
// fail-closed: when no stdio factory is registered (the default, because the
// platform owner has not set mcp.allow_stdio), CreateServer refuses to create a
// stdio server. This is the gate that prevents a tenant admin (org_admin, which
// has mcp:create) from spawning arbitrary local subprocesses on the gateway host.
func TestCreateServer_StdioDisabledByDefault(t *testing.T) {
	svc := NewMCPService(NewMCPRepo(testDB, testDialect), NewRegistry(), config.MCPConfig{}, nil)
	// Default MCPConfig has AllowStdio=false and no stdio factory is registered,
	// mirroring the production default.

	srv := &MCPServer{
		Name:          "test-stdio-disabled",
		TransportType: "stdio",
		StdioConfig:   []byte(`{"command":"/bin/echo"}`),
	}
	err := svc.CreateServer(testCtx, srv)
	if err == nil {
		t.Fatal("expected CreateServer to reject stdio when factory is not registered, got nil")
	}
	if !strings.Contains(err.Error(), "stdio") || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("expected an stdio-not-enabled error, got %v", err)
	}
}

// TestMCPConfig_AllowStdioDefaultFalse confirms the secure-by-default posture
// at the config layer.
func TestMCPConfig_AllowStdioDefaultFalse(t *testing.T) {
	var cfg config.MCPConfig
	if cfg.AllowStdio {
		t.Fatal("AllowStdio must default to false (secure-by-default)")
	}
}
