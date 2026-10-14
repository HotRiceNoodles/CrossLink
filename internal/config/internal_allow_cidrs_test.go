package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateway.internal_allow_cidrs loading matrix: YAML, env override, precedence,
// empty/whitespace env, comma splitting, and default-empty.

const allowCIDRsYAML = `
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
gateway:
  internal_allow_cidrs:
    - "10.10.0.0/16"
    - "fd00::/8"
`

func writeAllowCIDRsConfig(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644))
	return dir
}

func TestLoad_InternalAllowCIDRs_FromYAML(t *testing.T) {
	dir := writeAllowCIDRsConfig(t, allowCIDRsYAML)
	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"10.10.0.0/16", "fd00::/8"}, cfg.Gateway.InternalAllowCIDRs)
}

func TestLoad_InternalAllowCIDRs_DefaultEmpty(t *testing.T) {
	dir := t.TempDir() // no config file at all
	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Empty(t, cfg.Gateway.InternalAllowCIDRs, "default must be empty = strict posture")
}

func TestLoad_InternalAllowCIDRs_EnvOverridesYAML(t *testing.T) {
	dir := writeAllowCIDRsConfig(t, allowCIDRsYAML)
	t.Setenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS", "192.168.0.0/16, 10.0.0.0/8")
	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.0.0/16", "10.0.0.0/8"}, cfg.Gateway.InternalAllowCIDRs,
		"env must win over the YAML value")
}

func TestLoad_InternalAllowCIDRs_NoEnvKeepsYAML(t *testing.T) {
	dir := writeAllowCIDRsConfig(t, allowCIDRsYAML)
	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"10.10.0.0/16", "fd00::/8"}, cfg.Gateway.InternalAllowCIDRs)
}

func TestLoad_InternalAllowCIDRs_EmptyEnvTreatedAsUnset(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		dir := writeAllowCIDRsConfig(t, allowCIDRsYAML)
		t.Setenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS", raw)
		cfg, err := Load(dir)
		require.NoError(t, err)
		assert.Equal(t, []string{"10.10.0.0/16", "fd00::/8"}, cfg.Gateway.InternalAllowCIDRs,
			"empty/whitespace env (%q) must leave the YAML value in place", raw)
	}
}

func TestLoad_InternalAllowCIDRs_CommaSplittingTrimsAndDropsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS", " 10.0.0.0/8 ,, 172.20.0.0/14 ,")
	cfg, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8", "172.20.0.0/14"}, cfg.Gateway.InternalAllowCIDRs)
}

// Config carries strings; semantic validation (only private subnets) lives in
// guardrail.SetInternalAllowlist at startup and is covered there. This pins
// that an INVALID entry still flows through config untouched — the startup
// validator is what rejects it, atomically.
func TestLoad_InternalAllowCIDRs_InvalidEntriesPassedThrough(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS", "127.0.0.0/8,not-a-cidr")
	cfg, err := Load(dir)
	require.NoError(t, err, "config layer must not validate CIDR semantics")
	assert.Equal(t, []string{"127.0.0.0/8", "not-a-cidr"}, cfg.Gateway.InternalAllowCIDRs)
}

// B5 provenance: the config records whether the static value came from env
// or YAML so the allowlist service can label the active source and compute
// the reset fallback correctly.
func TestLoad_InternalAllowCIDRs_FromEnvFlag(t *testing.T) {
	dir := t.TempDir()

	t.Run("env supplies value", func(t *testing.T) {
		t.Setenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS", "10.0.0.0/8")
		cfg, err := Load(dir)
		require.NoError(t, err)
		assert.True(t, cfg.Gateway.InternalAllowCIDRsFromEnv)
	})

	t.Run("yaml supplies value", func(t *testing.T) {
		t.Setenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS", "")
		path := filepath.Join(dir, "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("gateway:\n  internal_allow_cidrs:\n    - 10.5.0.0/16\n"), 0o644))
		cfg, err := Load(dir)
		require.NoError(t, err)
		assert.False(t, cfg.Gateway.InternalAllowCIDRsFromEnv)
		assert.Equal(t, []string{"10.5.0.0/16"}, cfg.Gateway.InternalAllowCIDRs)
	})

	t.Run("nothing set", func(t *testing.T) {
		t.Setenv("CL_GATEWAY_INTERNAL_ALLOW_CIDRS", "")
		cfg, err := Load(t.TempDir())
		require.NoError(t, err)
		assert.False(t, cfg.Gateway.InternalAllowCIDRsFromEnv)
		assert.Empty(t, cfg.Gateway.InternalAllowCIDRs)
	})
}
