package config

// U-06 regression: smtp.* and the non-heartbeat license.* keys were
// documented as env-overridable but had no SetDefault — AutomaticEnv only
// resolves registered keys, so CL_SMTP_*/CL_LICENSE_* were silently ignored
// in pure-env (Docker) deployments.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_SMTPAndLicenseEnvOverride(t *testing.T) {
	dir := t.TempDir()
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

	t.Setenv("CL_SMTP_HOST", "smtp.example.com")
	t.Setenv("CL_SMTP_PORT", "465")
	t.Setenv("CL_SMTP_USERNAME", "mailer")
	t.Setenv("CL_SMTP_PASSWORD", "secret")
	t.Setenv("CL_SMTP_FROM", "no-reply@example.com")
	t.Setenv("CL_LICENSE_SERVER_URL", "https://lic.example.com")
	t.Setenv("CL_LICENSE_LICENSE_KEY", "key-1")
	t.Setenv("CL_LICENSE_LICENSE_FILE", "/var/lic/token")
	t.Setenv("CL_LICENSE_OFFLINE_FILE", "/var/lic/offline")
	t.Setenv("CL_LICENSE_NODE_ID", "node-7")

	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "smtp.example.com", cfg.SMTP.Host, "CL_SMTP_HOST must override smtp.host")
	assert.Equal(t, 465, cfg.SMTP.Port, "CL_SMTP_PORT must override smtp.port")
	assert.Equal(t, "mailer", cfg.SMTP.Username)
	assert.Equal(t, "secret", cfg.SMTP.Password)
	assert.Equal(t, "no-reply@example.com", cfg.SMTP.From)
	assert.Equal(t, "https://lic.example.com", cfg.License.ServerURL, "CL_LICENSE_SERVER_URL must override license.server_url")
	assert.Equal(t, "key-1", cfg.License.LicenseKey)
	assert.Equal(t, "/var/lic/token", cfg.License.LicenseFilePath)
	assert.Equal(t, "/var/lic/offline", cfg.License.OfflineFilePath)
	assert.Equal(t, "node-7", cfg.License.NodeID)
}

func TestLoad_SMTPDefaultsKeepYAMLPath(t *testing.T) {
	dir := t.TempDir()
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
smtp:
  host: yaml-smtp.internal
  port: 2525
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644))
	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "yaml-smtp.internal", cfg.SMTP.Host, "YAML must still win over the empty default")
	assert.Equal(t, 2525, cfg.SMTP.Port)
}
