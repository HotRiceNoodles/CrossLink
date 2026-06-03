package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(""), 0644)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("default port = %d, want 8080", cfg.Server.Port)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("default logging.level = %q, want %q", cfg.Logging.Level, "info")
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("default logging.format = %q, want %q", cfg.Logging.Format, "json")
	}
	if cfg.RateLimit.RPM != 0 {
		t.Errorf("default rpm = %d, want 0 (disabled)", cfg.RateLimit.RPM)
	}
	if cfg.RateLimit.TPM != 0 {
		t.Errorf("default tpm = %d, want 0 (disabled)", cfg.RateLimit.TPM)
	}
	if cfg.Admin.TokenExpiry != 24 {
		t.Errorf("default token_expiry = %d, want 24", cfg.Admin.TokenExpiry)
	}
	if cfg.Database.Host != "localhost" {
		t.Errorf("default db host = %q, want localhost", cfg.Database.Host)
	}
	if cfg.Redis.Port != 6379 {
		t.Errorf("default redis port = %d, want 6379", cfg.Redis.Port)
	}
}

func TestLoad_YAMLOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
server:
  port: 9090
logging:
  level: debug
  format: text
rate_limit:
  rpm: 120
  tpm: 200000
admin:
  username: superadmin
`
	os.WriteFile(cfgPath, []byte(content), 0644)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Server.Port != 9090 {
		t.Errorf("port = %d, want 9090", cfg.Server.Port)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("level = %q, want debug", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("format = %q, want text", cfg.Logging.Format)
	}
	if cfg.RateLimit.RPM != 120 {
		t.Errorf("rpm = %d, want 120", cfg.RateLimit.RPM)
	}
	if cfg.RateLimit.TPM != 200000 {
		t.Errorf("tpm = %d, want 200000", cfg.RateLimit.TPM)
	}
	if cfg.Admin.Username != "superadmin" {
		t.Errorf("admin username = %q, want superadmin", cfg.Admin.Username)
	}
	// Unchanged defaults
	if cfg.Redis.Port != 6379 {
		t.Errorf("redis port = %d, want default 6379", cfg.Redis.Port)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	cfg, err := Load("/nonexistent/path")
	if err != nil {
		t.Fatalf("Load() with missing file should not error, got: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config with defaults")
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("default port = %d, want 8080", cfg.Server.Port)
	}
}

func TestDSN(t *testing.T) {
	d := DatabaseConfig{
		Driver:   "postgres",
		Host:     "db.example.com",
		Port:     5433,
		User:     "testuser",
		Password: "secret",
		DBName:   "testdb",
		SSLMode:  "require",
	}
	expected := "host=db.example.com port=5433 user=testuser password=secret dbname=testdb sslmode=require"
	if d.DSN() != expected {
		t.Errorf("DSN() = %q, want %q", d.DSN(), expected)
	}
}

func TestDSNURL(t *testing.T) {
	d := DatabaseConfig{
		Driver:   "postgres",
		Host:     "db.example.com",
		Port:     5433,
		User:     "testuser",
		Password: "s@cr!t",
		DBName:   "testdb",
		SSLMode:  "require",
	}
	expected := "postgres://testuser:s@cr!t@db.example.com:5433/testdb?sslmode=require"
	if d.DSNURL() != expected {
		t.Errorf("DSNURL() = %q, want %q", d.DSNURL(), expected)
	}
}

func TestRedisAddr(t *testing.T) {
	r := RedisConfig{Host: "redis.example.com", Port: 6380}
	if r.Addr() != "redis.example.com:6380" {
		t.Errorf("Addr() = %q, want redis.example.com:6380", r.Addr())
	}
}
