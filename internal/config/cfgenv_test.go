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
