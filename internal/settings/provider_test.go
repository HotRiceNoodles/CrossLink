package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSettingsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SystemSetting{}))
	return db
}

func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.SMTP.Host = "yaml-smtp.example.com"
	cfg.SMTP.Port = 587
	cfg.SMTP.From = "yaml@example.com"
	cfg.Gateway.BaseURL = "https://yaml-gw.example.com"
	cfg.CORS.AllowedOrigins = []string{"https://yaml.example.com"}
	cfg.RateLimit.RPM = 100
	cfg.RateLimit.TPM = 100000
	cfg.RateLimit.Reservation = 2000
	cfg.RateLimit.FailClosed = true
	cfg.Captcha.Enabled = false
	cfg.Captcha.TrustDays = 7
	cfg.MCP.MaxServers = 3
	cfg.MCP.ToolCacheTTL = 5 * time.Minute
	cfg.MCP.RequestTimeout = 30 * time.Second
	cfg.DataLens.Retention.HourlyDays = 90
	cfg.GuardrailAlert.Concurrency = 8
	cfg.IPBinding.NotifyCooldownSeconds = 300
	return cfg
}

// D8 contract: with no DB rows the snapshot equals the yaml/env baseline.
func TestProvider_NoRowsEqualsBaseline(t *testing.T) {
	db := setupSettingsTestDB(t)
	cfg := testConfig()
	p := NewProvider(db, cfg, nil)

	s := p.Get()
	assert.Equal(t, cfg.SMTP, s.SMTP)
	assert.Equal(t, cfg.Gateway.BaseURL, s.GatewayBaseURL)
	assert.Equal(t, cfg.CORS.AllowedOrigins, s.CORSAllowed)
	assert.Equal(t, cfg.RateLimit, s.RateLimit)
	assert.Equal(t, cfg.MCP.MaxServers, s.MCP.MaxServers)
	assert.Equal(t, cfg.MCP.ToolCacheTTL, s.MCP.ToolCacheTTL)
	for _, d := range Registry {
		assert.Equal(t, SourceDefault, s.Source(d.Name), d.Name)
	}
}

func TestProvider_DBRowsOverrideBaseline(t *testing.T) {
	db := setupSettingsTestDB(t)
	p := NewProvider(db, testConfig(), nil)

	rows := map[string]string{
		KeySMTPHost:         "db-smtp.example.com",
		KeySMTPPort:         "465",
		KeyCORSOrigins:      `["https://a.example.com","https://b.example.com"]`,
		KeyRateLimitRPM:     "600",
		KeyRateLimitFail:    "false",
		KeyCaptchaEnabled:   "true",
		KeySliderTolerance:  "5.5",
		KeyMCPToolCacheTTL:  "120",
		KeyMCPRequestTTL:    "45",
		KeyDataLensHourly:   "30",
		KeyIPBCooldown:      "600",
		KeyGatewayBaseURL:   "https://db-gw.example.com",
		KeyGAConcurrency:    "16",
		KeyGAContentPreview: "false",
	}
	for k, v := range rows {
		require.NoError(t, db.Create(&model.SystemSetting{Key: k, Value: v}).Error)
	}
	require.NoError(t, p.RefreshNow(context.Background()))

	s := p.Get()
	assert.Equal(t, "db-smtp.example.com", s.SMTP.Host)
	assert.Equal(t, 465, s.SMTP.Port)
	assert.Equal(t, "yaml@example.com", s.SMTP.From, "untouched key keeps yaml value")
	assert.Equal(t, []string{"https://a.example.com", "https://b.example.com"}, s.CORSAllowed)
	assert.Equal(t, 600, s.RateLimit.RPM)
	assert.False(t, s.RateLimit.FailClosed)
	assert.True(t, s.Captcha.Enabled)
	assert.InDelta(t, 5.5, s.Captcha.Slider.TolerancePx, 0.001)
	assert.Equal(t, 2*time.Minute, s.MCP.ToolCacheTTL, "duration_seconds → Duration")
	assert.Equal(t, 45*time.Second, s.MCP.RequestTimeout)
	assert.Equal(t, 30, s.DataLens.Retention.HourlyDays)
	assert.Equal(t, 600, s.IPBinding.NotifyCooldownSeconds)
	assert.Equal(t, "https://db-gw.example.com", s.GatewayBaseURL)
	assert.Equal(t, 16, s.GuardrailAlert.Concurrency)
	assert.False(t, s.GuardrailAlert.ContentPreview)

	for k := range rows {
		assert.Equal(t, SourceDB, s.Source(k), k)
	}
	assert.Equal(t, SourceDefault, s.Source(KeySMTPFrom))
}

func TestProvider_DeletedRowFallsBackToBaseline(t *testing.T) {
	db := setupSettingsTestDB(t)
	cfg := testConfig()
	p := NewProvider(db, cfg, nil)

	require.NoError(t, db.Create(&model.SystemSetting{Key: KeyRateLimitRPM, Value: "600"}).Error)
	require.NoError(t, p.RefreshNow(context.Background()))
	assert.Equal(t, 600, p.RateLimit().RPM)

	require.NoError(t, db.Where("key = ?", KeyRateLimitRPM).Delete(&model.SystemSetting{}).Error)
	require.NoError(t, p.RefreshNow(context.Background()))
	assert.Equal(t, cfg.RateLimit.RPM, p.RateLimit().RPM)
	assert.Equal(t, SourceDefault, p.Get().Source(KeyRateLimitRPM))
}

func TestProvider_BadRowKeepsBaselineAndMarksError(t *testing.T) {
	db := setupSettingsTestDB(t)
	cfg := testConfig()
	p := NewProvider(db, cfg, nil)

	require.NoError(t, db.Create(&model.SystemSetting{Key: KeyRateLimitRPM, Value: "not-a-number"}).Error)
	require.NoError(t, db.Create(&model.SystemSetting{Key: KeyCORSOrigins, Value: `{invalid json`}).Error)
	require.NoError(t, p.RefreshNow(context.Background()))

	s := p.Get()
	assert.Equal(t, cfg.RateLimit.RPM, s.RateLimit.RPM)
	assert.Equal(t, SourceDBError, s.Source(KeyRateLimitRPM))
	assert.Equal(t, cfg.CORS.AllowedOrigins, s.CORSAllowed)
	assert.Equal(t, SourceDBError, s.Source(KeyCORSOrigins))
}

func TestProvider_SMTPPasswordEncryptedRoundTrip(t *testing.T) {
	db := setupSettingsTestDB(t)
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)
	raw := make([]byte, cp.CipherKeySize())
	_, err = rand.Read(raw)
	require.NoError(t, err)
	store, err := secret.NewEncryptedDBStore(base64.StdEncoding.EncodeToString(raw), cp)
	require.NoError(t, err)

	p := NewProvider(db, testConfig(), store)
	enc, err := store.Encrypt("mail-password")
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.SystemSetting{Key: KeySMTPPassword, Value: enc}).Error)
	require.NoError(t, p.RefreshNow(context.Background()))

	assert.Equal(t, "mail-password", p.SMTP().Password)
	assert.Equal(t, SourceDB, p.Get().Source(KeySMTPPassword))
}

func TestProvider_SMTPPasswordEncryptedButKeyless(t *testing.T) {
	db := setupSettingsTestDB(t)
	cfg := testConfig()
	cfg.SMTP.Password = "yaml-pass"
	p := NewProvider(db, cfg, nil)

	require.NoError(t, db.Create(&model.SystemSetting{Key: KeySMTPPassword, Value: "enc2://opaque"}).Error)
	require.NoError(t, p.RefreshNow(context.Background()))

	assert.Equal(t, "yaml-pass", p.SMTP().Password, "baseline kept")
	assert.Equal(t, SourceDBError, p.Get().Source(KeySMTPPassword))
}

func TestProvider_SMTPPasswordPlaintextRowWhenKeyless(t *testing.T) {
	db := setupSettingsTestDB(t)
	p := NewProvider(db, testConfig(), nil)

	require.NoError(t, db.Create(&model.SystemSetting{Key: KeySMTPPassword, Value: "plain-mail-pass"}).Error)
	require.NoError(t, p.RefreshNow(context.Background()))

	assert.Equal(t, "plain-mail-pass", p.SMTP().Password)
	assert.Equal(t, SourceDB, p.Get().Source(KeySMTPPassword))
}

func TestProvider_SubscribersFireOnEverySwap(t *testing.T) {
	db := setupSettingsTestDB(t)
	p := NewProvider(db, testConfig(), nil)

	var calls atomic.Int64
	p.Subscribe(func(s *Snapshot) { calls.Add(1) })
	require.NoError(t, db.Create(&model.SystemSetting{Key: KeyRateLimitRPM, Value: "42"}).Error)

	require.NoError(t, p.RefreshNow(context.Background()))
	assert.Equal(t, int64(1), calls.Load(), "subscribers see post-subscribe swaps (initial load predates Subscribe)")

	assert.Equal(t, 42, p.RateLimit().RPM)
}

func TestProvider_LoadErrorKeepsPreviousSnapshot(t *testing.T) {
	db := setupSettingsTestDB(t)
	p := NewProvider(db, testConfig(), nil)
	require.NoError(t, db.Create(&model.SystemSetting{Key: KeyRateLimitRPM, Value: "42"}).Error)
	require.NoError(t, p.RefreshNow(context.Background()))

	// Drop the table to force a load error; snapshot must survive.
	require.NoError(t, db.Migrator().DropTable("system_settings"))
	require.Error(t, p.RefreshNow(context.Background()))
	assert.Equal(t, 42, p.RateLimit().RPM)
}

func TestRegistry_AllKeysUniqueAndInSection(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Registry {
		assert.False(t, seen[d.Name], "duplicate key %s", d.Name)
		seen[d.Name] = true
		assert.NotEmpty(t, d.Section, d.Name)
		assert.NotEmpty(t, d.Type, d.Name)
	}
	// Sensitive keys must never be restart-labeled false AND sensitive? (no
	// constraint) — but every key must be in RegistryMap.
	for _, d := range Registry {
		_, ok := RegistryMap[d.Name]
		assert.True(t, ok, d.Name)
	}
	assert.Len(t, RegistryMap, len(Registry))
}

func TestRangeValidation(t *testing.T) {
	port := RegistryMap[KeySMTPPort]
	assert.True(t, port.hasRange)
	// spot-check bounds semantics via fields (handler applies them)
	assert.Equal(t, float64(1), port.Min)
	assert.Equal(t, float64(65535), port.Max)
	rpm := RegistryMap[KeyRateLimitRPM]
	assert.True(t, rpm.hasRange && rpm.Max == 0, "Max 0 = no upper bound")
}
