package config

import (
	"os"
	"testing"
	"time"
)

func TestPoolEnvOverrideE2E(t *testing.T) {
	os.Setenv("CL_DATABASE_CONN_MAX_LIFETIME", "1h")
	os.Setenv("CL_DATABASE_CONN_MAX_IDLE_TIME", "30m")
	os.Setenv("CL_DATABASE_MAX_IDLE_CONNS", "60")
	defer func() {
		os.Unsetenv("CL_DATABASE_CONN_MAX_LIFETIME")
		os.Unsetenv("CL_DATABASE_CONN_MAX_IDLE_TIME")
		os.Unsetenv("CL_DATABASE_MAX_IDLE_CONNS")
	}()
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Logf("lifetime=%v idle_time=%v max_idle=%d", cfg.Database.ConnMaxLifetime, cfg.Database.ConnMaxIdleTime, cfg.Database.MaxIdleConns)
	if cfg.Database.ConnMaxLifetime != time.Hour {
		t.Fatalf("CL_DATABASE_CONN_MAX_LIFETIME=1h not applied: got %v", cfg.Database.ConnMaxLifetime)
	}
}

// TestEnvOnly_BudgetMetadataTiming (r71 follow-up): the fail-safe timing
// keys must be honored env-only — the r70 finding was that without a viper
// SetDefault registration, CL_GATEWAY_BUDGET_METADATA_* was silently inert
// (viper's AutomaticEnv only resolves keys it knows). Pins the fix.
func TestEnvOnly_BudgetMetadataTiming(t *testing.T) {
	t.Setenv("CL_GATEWAY_BUDGET_METADATA_STALE_WINDOW", "90s")
	t.Setenv("CL_GATEWAY_BUDGET_METADATA_TTL", "6s")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Gateway.BudgetMetadataStaleWindow != "90s" {
		t.Fatalf("STALE_WINDOW env-only not honored: got %q", cfg.Gateway.BudgetMetadataStaleWindow)
	}
	if cfg.Gateway.BudgetMetadataTTL != "6s" {
		t.Fatalf("TTL env-only not honored: got %q", cfg.Gateway.BudgetMetadataTTL)
	}
}

// Without the env, the registered defaults must surface (not empty strings)
// so the cache-side fallbacks are an explicit choice, not an unset accident.
func TestDefaults_BudgetMetadataTiming(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Gateway.BudgetMetadataStaleWindow != "10m" {
		t.Fatalf("default stale window = %q, want 10m", cfg.Gateway.BudgetMetadataStaleWindow)
	}
	if cfg.Gateway.BudgetMetadataTTL != "30s" {
		t.Fatalf("default ttl = %q, want 30s", cfg.Gateway.BudgetMetadataTTL)
	}
}

// r78 availability bounds: both lookup-timeout keys must honor env-only
// overrides (same AutomaticEnv registration rationale as above) and default
// to a conservative 2s.
func TestEnvOnly_LookupTimeouts(t *testing.T) {
	t.Setenv("CL_GATEWAY_BUDGET_METADATA_LOOKUP_TIMEOUT", "500ms")
	t.Setenv("CL_GATEWAY_RESOLVE_LOOKUP_TIMEOUT", "750ms")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Gateway.BudgetMetadataLookupTimeout != "500ms" {
		t.Fatalf("budget lookup timeout env-only not honored: got %q", cfg.Gateway.BudgetMetadataLookupTimeout)
	}
	if cfg.Gateway.ResolveLookupTimeout != "750ms" {
		t.Fatalf("resolve lookup timeout env-only not honored: got %q", cfg.Gateway.ResolveLookupTimeout)
	}
}

func TestDefaults_LookupTimeouts(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Gateway.BudgetMetadataLookupTimeout != "2s" {
		t.Fatalf("default budget lookup timeout = %q, want 2s", cfg.Gateway.BudgetMetadataLookupTimeout)
	}
	if cfg.Gateway.ResolveLookupTimeout != "2s" {
		t.Fatalf("default resolve lookup timeout = %q, want 2s", cfg.Gateway.ResolveLookupTimeout)
	}
}
