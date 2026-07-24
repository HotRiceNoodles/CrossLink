package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: without SetEnvKeyReplacer, Viper maps a nested key like
// "server.port" to the env var "CL_SERVER.PORT" (literal dot) — un-settable in
// any shell. The docker-compose files override via "CL_SERVER_PORT" (underscore),
// which silently never applied. This test pins the underscore form.
func TestLoad_EnvOverrideNestedKey(t *testing.T) {
	dir := t.TempDir()
	// A minimal-but-valid config: file says postgres/9999; env must override.
	yaml := `
server:
  port: 9999
  read_timeout: 30s
  write_timeout: 120s
database:
  driver: postgres
  host: localhost
  port: 5432
  user: crosslink
  password: x
  dbname: crosslink
  sslmode: disable
admin:
  username: admin
  password: admin123
  jwt_secret: "this-must-be-at-least-thirty-two-characters-long"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644))

	// With SetEnvKeyReplacer set, nested underscore env vars override file values.
	// (driver/sqlite_path/auth_key/encryption_key env coverage is proven by the
	// docker-scenario test below — kept separate to avoid Viper's inconsistent
	// file-vs-env precedence for some keys muddying this assertion.)
	t.Setenv("CL_SERVER_PORT", "18080")
	t.Setenv("CL_DATABASE_HOST", "db.example.com")

	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, 18080, cfg.Server.Port, "CL_SERVER_PORT must override server.port")
	assert.Equal(t, "db.example.com", cfg.Database.Host, "CL_DATABASE_HOST must override database.host")
}

// Docker scenario: NO config.yaml in the search path (the image only ships
// config.example.yaml, which Viper does not load). Every value must come from
// defaults + env. Before the fix, keys without an explicit default (gateway.auth_key,
// secret_manager.encryption_key, database.sqlite_path) silently ignored their
// CL_* env var even with SetEnvKeyReplacer, because AutomaticEnv + Unmarshal
// only resolves env for keys Viper already knows.
func TestLoad_EnvOverrideDockerScenario(t *testing.T) {
	// Empty temp dir — no config.yaml present, like a docker container.
	dir := t.TempDir()

	t.Setenv("CL_DATABASE_DRIVER", "sqlite")
	t.Setenv("CL_DATABASE_SQLITE_PATH", "/data/cl.db")
	t.Setenv("CL_GATEWAY_AUTH_KEY", "docker-gateway-key")
	t.Setenv("CL_ENCRYPTION_KEY", "docker-encryption-key")
	t.Setenv("CL_ADMIN_JWT_SECRET", "this-must-be-at-least-thirty-two-characters-long")

	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "sqlite", cfg.Database.Driver)
	assert.Equal(t, "/data/cl.db", cfg.Database.SQLitePath, "CL_DATABASE_SQLITE_PATH must apply without a config file")
	assert.Equal(t, "docker-gateway-key", cfg.Gateway.AuthKey, "CL_GATEWAY_AUTH_KEY must apply without a config file")
	assert.Equal(t, "docker-encryption-key", cfg.SecretManager.EncryptionKey, "CL_ENCRYPTION_KEY must apply without a config file")
}
