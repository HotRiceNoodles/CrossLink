package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Mirrors the isolated gateway deployment: a configs/config.yaml exists with
// a database: section (driver/host/port/user/dbname/sslmode) but NO pool
// keys, and CL_DATABASE_* env vars carry the pool overrides. Env must beat
// both the file and the dialect defaults.
func TestPoolEnvOverrideWithYamlPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "server:\n  port: 18091\ndatabase:\n  driver: postgres\n  host: 127.0.0.1\n  port: 15432\n  user: postgres\n  dbname: crosslink\n  sslmode: disable\n"
	if err := os.WriteFile(filepath.Join(dir, "configs", "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CL_DATABASE_CONN_MAX_LIFETIME", "1h")
	t.Setenv("CL_DATABASE_CONN_MAX_IDLE_TIME", "30m")
	t.Setenv("CL_DATABASE_MAX_IDLE_CONNS", "60")

	t.Chdir(dir)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Database.ConnMaxLifetime != time.Hour {
		t.Fatalf("CL_DATABASE_CONN_MAX_LIFETIME=1h not applied with yaml present: got %v", cfg.Database.ConnMaxLifetime)
	}
	if cfg.Database.ConnMaxIdleTime != 30*time.Minute {
		t.Fatalf("CL_DATABASE_CONN_MAX_IDLE_TIME=30m not applied: got %v", cfg.Database.ConnMaxIdleTime)
	}
	if cfg.Database.MaxIdleConns != 60 {
		t.Fatalf("CL_DATABASE_MAX_IDLE_CONNS=60 not applied: got %d", cfg.Database.MaxIdleConns)
	}
}
