package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupReadinessTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SystemSetting{}, &model.Provider{}, &model.APIKey{}))
	return db
}

func newReadinessCheckerForTest(db *gorm.DB, cfg *config.Config) *ReadinessChecker {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return NewReadinessChecker(db, cfg)
}

func findCheck(report *ReadinessReport, id string) *ReadinessCheck {
	for i := range report.Checks {
		if report.Checks[i].ID == id {
			return &report.Checks[i]
		}
	}
	return nil
}

func TestReadiness_FreshInstall(t *testing.T) {
	db := setupReadinessTestDB(t)
	report, err := newReadinessCheckerForTest(db, nil).Evaluate(context.Background())
	require.NoError(t, err)

	// No setup row + no providers → wizard should be forced.
	assert.True(t, report.SetupNeeded, "fresh install must trigger setup wizard")

	assert.Equal(t, SeverityWarn, findCheck(report, "encryption_key").Severity, "no key, no providers → warn")
	assert.Equal(t, SeverityWarn, findCheck(report, "setup_wizard_status").Severity, "status row absent → warn")
	assert.Equal(t, SeverityWarn, findCheck(report, "providers_configured").Severity)
	assert.Equal(t, SeverityInfo, findCheck(report, "api_key_exists").Severity)
	assert.Equal(t, SeverityInfo, findCheck(report, "timezone_configured").Severity)
	assert.Equal(t, SeverityOK, findCheck(report, "boot_security_guards").Severity)

	// Summary counts must match check severities.
	total := report.Summary.Danger + report.Summary.Warn + report.Summary.Info + report.Summary.OK
	assert.Equal(t, len(report.Checks), total)

	// Checks are ordered danger → warn → info → ok.
	order := severityOrder
	for i := 1; i < len(report.Checks); i++ {
		assert.LessOrEqual(t, order[report.Checks[i-1].Severity], order[report.Checks[i].Severity],
			"checks must be sorted by severity")
	}
}

func TestReadiness_PlaintextProvidersWithoutKeyIsDanger(t *testing.T) {
	db := setupReadinessTestDB(t)
	require.NoError(t, db.Create(&model.Provider{Name: "p1", DisplayName: "P1", AdapterType: "openai", BaseURL: "https://api.example.com", APIKey: "sk-plaintext"}).Error)

	report, err := newReadinessCheckerForTest(db, nil).Evaluate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SeverityDanger, findCheck(report, "encryption_key").Severity)
	assert.Equal(t, map[string]any{"provider_count": 1}, findCheck(report, "encryption_key").DetailParams)
	assert.Equal(t, 1, report.Summary.Danger)
}

func TestReadiness_EncryptedAndReferenceSecretsNotPlaintext(t *testing.T) {
	db := setupReadinessTestDB(t)
	require.NoError(t, db.Create(&model.SystemSetting{Key: "encryption_key", Value: "dGVzdA=="}).Error)
	require.NoError(t, db.Create(&model.Provider{Name: "p1", DisplayName: "P1", AdapterType: "openai", BaseURL: "https://a.example.com", APIKey: "enc2://ciphertext"}).Error)
	require.NoError(t, db.Create(&model.Provider{Name: "p2", DisplayName: "P2", AdapterType: "openai", BaseURL: "https://b.example.com", APIKey: "vault://path/to/secret"}).Error)
	require.NoError(t, db.Create(&model.Provider{Name: "p3", DisplayName: "P3", AdapterType: "openai", BaseURL: "https://c.example.com", APIKey: ""}).Error)

	report, err := newReadinessCheckerForTest(db, nil).Evaluate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SeverityOK, findCheck(report, "encryption_key").Severity)
}

func TestReadiness_ConfigKeyCountsAsActive(t *testing.T) {
	db := setupReadinessTestDB(t)
	cfg := &config.Config{}
	cfg.SecretManager.EncryptionKey = "dGVzdA=="
	report, err := newReadinessCheckerForTest(db, cfg).Evaluate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SeverityOK, findCheck(report, "encryption_key").Severity)
}

func TestReadiness_ExistingInstallNotForced(t *testing.T) {
	db := setupReadinessTestDB(t)
	require.NoError(t, db.Create(&model.Provider{Name: "p1", DisplayName: "P1", AdapterType: "openai", BaseURL: "https://api.example.com", APIKey: "sk-x"}).Error)

	report, err := newReadinessCheckerForTest(db, nil).Evaluate(context.Background())
	require.NoError(t, err)
	assert.False(t, report.SetupNeeded, "providers exist (old onboarding done) → never force wizard")
}

func TestReadiness_SetupStatusVariants(t *testing.T) {
	db := setupReadinessTestDB(t)
	require.NoError(t, db.Create(&model.SystemSetting{Key: "setup_wizard_status", Value: "skipped"}).Error)
	report, err := newReadinessCheckerForTest(db, nil).Evaluate(context.Background())
	require.NoError(t, err)
	assert.False(t, report.SetupNeeded, "skipped → never re-force")
	assert.Equal(t, SeverityInfo, findCheck(report, "setup_wizard_status").Severity)

	db2 := setupReadinessTestDB(t)
	require.NoError(t, db2.Create(&model.SystemSetting{Key: "setup_wizard_status", Value: "done"}).Error)
	report2, err := newReadinessCheckerForTest(db2, nil).Evaluate(context.Background())
	require.NoError(t, err)
	assert.False(t, report2.SetupNeeded)
	assert.Equal(t, SeverityOK, findCheck(report2, "setup_wizard_status").Severity)
}

func TestReadiness_TimezoneMatrix(t *testing.T) {
	// Both unset → info.
	db := setupReadinessTestDB(t)
	report, err := newReadinessCheckerForTest(db, nil).Evaluate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SeverityInfo, findCheck(report, "timezone_configured").Severity)

	// DB row set, yaml empty → ok.
	require.NoError(t, db.Create(&model.SystemSetting{Key: "stats_timezone", Value: "Asia/Shanghai"}).Error)
	report, err = newReadinessCheckerForTest(db, nil).Evaluate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SeverityOK, findCheck(report, "timezone_configured").Severity)

	// Both set and differing → warn (DB shadows yaml).
	cfg := &config.Config{}
	cfg.Database.Timezone = "UTC"
	report, err = newReadinessCheckerForTest(db, cfg).Evaluate(context.Background())
	require.NoError(t, err)
	c := findCheck(report, "timezone_configured")
	assert.Equal(t, SeverityWarn, c.Severity)
	assert.Equal(t, "Asia/Shanghai", c.DetailParams["db_timezone"])
	assert.Equal(t, "UTC", c.DetailParams["config_timezone"])

	// Both set and equal → ok.
	cfg.Database.Timezone = "Asia/Shanghai"
	report, err = newReadinessCheckerForTest(db, cfg).Evaluate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SeverityOK, findCheck(report, "timezone_configured").Severity)
}

func TestReadiness_SMTPAndMiscConfigChecks(t *testing.T) {
	db := setupReadinessTestDB(t)
	cfg := &config.Config{}
	report, err := newReadinessCheckerForTest(db, cfg).Evaluate(context.Background())
	require.NoError(t, err)
	// Community build: unset SMTP is informational.
	assert.Equal(t, SeverityInfo, findCheck(report, "smtp_configured").Severity)
	assert.Equal(t, SeverityInfo, findCheck(report, "cors_allowed_origins").Severity)
	assert.Equal(t, SeverityInfo, findCheck(report, "trusted_proxies").Severity)
	assert.Equal(t, SeverityInfo, findCheck(report, "gateway_base_url").Severity)

	// Configured values flip them to ok.
	cfg.SMTP.Host = "smtp.example.com"
	cfg.CORS.AllowedOrigins = []string{"https://dash.example.com"}
	cfg.Server.TrustedProxies = []string{"127.0.0.1"}
	cfg.Gateway.BaseURL = "https://gw.example.com"
	report, err = newReadinessCheckerForTest(db, cfg).Evaluate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SeverityOK, findCheck(report, "smtp_configured").Severity)
	assert.Equal(t, SeverityOK, findCheck(report, "cors_allowed_origins").Severity)
	assert.Equal(t, SeverityOK, findCheck(report, "trusted_proxies").Severity)
	assert.Equal(t, SeverityOK, findCheck(report, "gateway_base_url").Severity)
}

func TestReadiness_ContentLog(t *testing.T) {
	db := setupReadinessTestDB(t)
	require.NoError(t, db.Create(&model.SystemSetting{Key: "log_content", Value: "true"}).Error)
	report, err := newReadinessCheckerForTest(db, nil).Evaluate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SeverityInfo, findCheck(report, "content_log_enabled").Severity)
}

func TestSetupNeeded_FalseOnError(t *testing.T) {
	// DB without migrated tables must not blow up or force the wizard.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	assert.False(t, newReadinessCheckerForTest(db, nil).SetupNeeded(context.Background()))
}

func TestReadinessHandler_Report(t *testing.T) {
	db := setupReadinessTestDB(t)
	h := NewReadinessHandler(newReadinessCheckerForTest(db, nil))

	c, w := newTestContext(t, http.MethodGet, "/admin/api/system/readiness", nil)
	h.Report(c)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data ReadinessReport `json:"data"`
	}
	decodeResponse(t, w, &resp)
	assert.NotEmpty(t, resp.Data.Checks)
	assert.Equal(t, len(resp.Data.Checks),
		resp.Data.Summary.Danger+resp.Data.Summary.Warn+resp.Data.Summary.Info+resp.Data.Summary.OK)
}
