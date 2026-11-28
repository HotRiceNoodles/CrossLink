package admin

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func summaryCfg() *config.Config {
	return &config.Config{
		Billing: config.BillingConfig{Currency: "CNY"},
		Crypto:  config.CryptoConfig{Mode: "standard"},
		SecretManager: config.SecretManagerConfig{
			EncryptionKey: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		},
		Admin:    config.AdminConfig{JWTSecret: strings.Repeat("j", 48), Password: "a-strong-install-password"},
		Database: config.DatabaseConfig{Driver: "postgres", Host: "db", Port: 5432, DBName: "crosslink"},
	}
}

func callSummary(t *testing.T, h *SystemHandler) map[string]any {
	t.Helper()
	c, w := newTestContext(t, http.MethodGet, "/admin/api/system/install-summary", nil)
	h.GetInstallSummary(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Data
}

func summaryItems(t *testing.T, data map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := data["items"].([]any)
	require.True(t, ok, "items missing")
	out := map[string]map[string]any{}
	for _, it := range raw {
		m := it.(map[string]any)
		out[m["param"].(string)] = m
	}
	return out
}

// newSummaryHandler wires the handler the way ProvideAdminHandlers does:
// the settings provider reads the SAME db the handler uses.
func newSummaryHandler(t *testing.T) *SystemHandler {
	t.Helper()
	db := setupSystemTestDB(t)
	cfg := summaryCfg()
	h := NewSystemHandler(db, nil, cfg.Admin, nil, nil, nil, nil, nil)
	h.SetFullConfig(cfg)
	h.SetSettingsProvider(settings.NewProvider(db, &config.Config{}, nil))
	return h
}

func TestInstallSummary_HealthyAndNeverLeaksSecrets(t *testing.T) {
	h := newSummaryHandler(t)
	data := callSummary(t, h)
	assert.Equal(t, true, data["healthy"])
	items := summaryItems(t, data)

	for _, p := range []string{"CL_ENCRYPTION_KEY", "CL_ADMIN_JWT_SECRET", "CL_ADMIN_PASSWORD"} {
		it, ok := items[p]
		require.True(t, ok, "missing %s", p)
		assert.Equal(t, true, it["secret"], "%s must be marked secret", p)
		assert.Equal(t, "configured", it["value"], "%s value must be a status word, not the secret", p)
	}
	// The strongest assertion: no secret material anywhere in the payload.
	cfg := summaryCfg()
	body, _ := json.Marshal(data)
	for _, secret := range []string{cfg.SecretManager.EncryptionKey, cfg.Admin.JWTSecret, cfg.Admin.Password} {
		assert.NotContains(t, string(body), secret, "a secret value leaked into the install summary")
	}
	assert.Equal(t, "standard (AES)", items["CL_CRYPTO_MODE"]["value"])
	assert.NotEmpty(t, items["CL_BILLING_CURRENCY"]["locked"], "currency must explain how it is locked")
}

// A declared-but-not-yet-written currency is reported as declared, NOT as
// locked: the write-once row does not exist yet.
func TestInstallSummary_DeclaredCurrencyIsNotLocked(t *testing.T) {
	h := newSummaryHandler(t)
	items := summaryItems(t, callSummary(t, h))
	assert.Equal(t, "CNY (declared; locked on first write)", items["CL_BILLING_CURRENCY"]["value"])
}

// Once the write-once row exists the summary shows the DB value and says so.
func TestInstallSummary_LockedCurrencyComesFromDatabase(t *testing.T) {
	h := newSummaryHandler(t)
	require.NoError(t, h.db.Create(&model.SystemSetting{
		Key: "billing_currency", Value: "USD", UpdatedAt: time.Now().UTC(),
	}).Error)
	require.NoError(t, h.settings.RefreshNow(t.Context()))
	items := summaryItems(t, callSummary(t, h))
	assert.Equal(t, "USD", items["CL_BILLING_CURRENCY"]["value"])
	assert.Equal(t, "database (locked)", items["CL_BILLING_CURRENCY"]["source"])
}

func TestInstallSummary_UnhealthyWhenMisconfigured(t *testing.T) {
	db := setupSystemTestDB(t)
	cfg := summaryCfg()
	cfg.Admin.JWTSecret = "" // boot guard would refuse this
	h := NewSystemHandler(db, nil, cfg.Admin, nil, nil, nil, nil, nil)
	h.SetFullConfig(cfg)
	h.SetSettingsProvider(settings.NewProvider(db, &config.Config{}, nil))
	data := callSummary(t, h)
	assert.Equal(t, false, data["healthy"])
	assert.Equal(t, "error", summaryItems(t, data)["CL_ADMIN_JWT_SECRET"]["status"])
}

func TestInstallSummary_NotChosenCurrency(t *testing.T) {
	db := setupSystemTestDB(t)
	cfg := summaryCfg()
	cfg.Billing.Currency = ""
	h := NewSystemHandler(db, nil, cfg.Admin, nil, nil, nil, nil, nil)
	h.SetFullConfig(cfg)
	h.SetSettingsProvider(settings.NewProvider(db, &config.Config{}, nil))
	assert.Equal(t, "not chosen yet", summaryItems(t, callSummary(t, h))["CL_BILLING_CURRENCY"]["value"])
}
