package admin

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func stdProviderForTest(t *testing.T) crypto.CryptoProvider {
	t.Helper()
	p, err := crypto.NewProvider("standard")
	require.NoError(t, err)
	return p
}

func newSetupHandlerForTest(t *testing.T, db *gorm.DB, cfg *config.Config) *SetupHandler {
	return NewSetupHandler(db, cfg, stdProviderForTest(t), nil, nil)
}

func TestSetupApply_WritesAllRowsAndHotAppliesTimezone(t *testing.T) {
	db := setupReadinessTestDB(t)
	cfg := &config.Config{}
	h := newSetupHandlerForTest(t, db, cfg)
	defer SetStatsTimezone("") // restore package state

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/setup/apply", map[string]any{
		"timezone":   "Asia/Shanghai",
		"base_url":   "https://1.2.4.8",
		"status":     "done",
	})
	h.Apply(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Data setupApplyResult `json:"data"`
	}
	decodeResponse(t, w, &resp)
	assert.Contains(t, resp.Data.Applied, "stats_timezone")
	assert.Contains(t, resp.Data.Applied, "gateway_base_url")
	assert.Equal(t, "done", resp.Data.Applied["setup_wizard_status"])
	assert.Contains(t, resp.Data.Applied, "setup_wizard_completed_at")
	assert.False(t, resp.Data.RestartRecommended, "no key generated → no restart needed")

	// Rows persisted.
	for key, want := range map[string]string{
		"stats_timezone":             "Asia/Shanghai",
		"gateway_base_url":           "https://1.2.4.8",
		"setup_wizard_status":        "done",
	} {
		var row model.SystemSetting
		require.NoError(t, db.Where("key = ?", key).First(&row).Error, key)
		assert.Equal(t, want, row.Value, key)
	}

	// Go-side stats timezone hot-applied.
	assert.Equal(t, "Asia/Shanghai", statsTZName)
}

func TestSetupApply_GeneratesEncryptionKeyWhenKeyless(t *testing.T) {
	db := setupReadinessTestDB(t)
	h := newSetupHandlerForTest(t, db, &config.Config{})

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/setup/apply", map[string]any{
		"generate_encryption_key": true,
		"status":                  "done",
	})
	h.Apply(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var row model.SystemSetting
	require.NoError(t, db.Where("key = ?", "encryption_key").First(&row).Error)
	raw, err := base64.StdEncoding.DecodeString(row.Value)
	require.NoError(t, err)
	assert.Len(t, raw, stdProviderForTest(t).CipherKeySize(), "32-byte AES key")

	var resp struct {
		Data setupApplyResult `json:"data"`
	}
	decodeResponse(t, w, &resp)
	assert.True(t, resp.Data.RestartRecommended, "process booted keyless (encStore nil) → restart until watcher picks it up")
	// Never leak the raw key in the response.
	assert.Equal(t, "generated", resp.Data.Applied["encryption_key"])
}

func TestSetupApply_KeyGenerationRefusedWhenKeyActive(t *testing.T) {
	// Active via config key.
	db := setupReadinessTestDB(t)
	cfg := &config.Config{}
	cfg.SecretManager.EncryptionKey = "dGVzdA=="
	h := newSetupHandlerForTest(t, db, cfg)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/setup/apply", map[string]any{"generate_encryption_key": true, "status": "done"})
	h.Apply(c)
	assert.Equal(t, http.StatusConflict, w.Code)

	// Active via DB row.
	db2 := setupReadinessTestDB(t)
	require.NoError(t, db2.Create(&model.SystemSetting{Key: "encryption_key", Value: "dGVzdA=="}).Error)
	h2 := newSetupHandlerForTest(t, db2, &config.Config{})
	c2, w2 := newTestContext(t, http.MethodPost, "/admin/api/system/setup/apply", map[string]any{"generate_encryption_key": true, "status": "done"})
	h2.Apply(c2)
	assert.Equal(t, http.StatusConflict, w2.Code)
}

func TestSetupApply_Validation(t *testing.T) {
	db := setupReadinessTestDB(t)
	h := newSetupHandlerForTest(t, db, &config.Config{})

	cases := []struct {
		name string
		body map[string]any
	}{
		{"bad timezone", map[string]any{"timezone": "Not/AZone", "status": "done"}},
		{"bad base_url scheme", map[string]any{"base_url": "ftp://x.example.com", "status": "done"}},
		{"internal base_url", map[string]any{"base_url": "http://127.0.0.1:8080", "status": "done"}},
		{"bad status", map[string]any{"status": "finished"}},
	}
	for _, tc := range cases {
		c, w := newTestContext(t, http.MethodPost, "/admin/api/system/setup/apply", tc.body)
		h.Apply(c)
		assert.Equal(t, http.StatusBadRequest, w.Code, tc.name)
	}
}

func TestSetupApply_PendingStatusSetsWizardRetriable(t *testing.T) {
	db := setupReadinessTestDB(t)
	h := newSetupHandlerForTest(t, db, &config.Config{})

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/setup/apply", map[string]any{"timezone": "UTC", "status": "pending"})
	h.Apply(c)
	require.Equal(t, http.StatusOK, w.Code)

	var row model.SystemSetting
	require.NoError(t, db.Where("key = ?", "setup_wizard_status").First(&row).Error)
	assert.Equal(t, "pending", row.Value)
	// No completed-at timestamp for pending.
	assert.Error(t, db.Where("key = ?", "setup_wizard_completed_at").First(&row).Error)
}

func TestSetupSkip(t *testing.T) {
	db := setupReadinessTestDB(t)
	h := newSetupHandlerForTest(t, db, &config.Config{})

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/setup/skip", nil)
	h.Skip(c)
	require.Equal(t, http.StatusOK, w.Code)

	var row model.SystemSetting
	require.NoError(t, db.Where("key = ?", "setup_wizard_status").First(&row).Error)
	assert.Equal(t, "skipped", row.Value)

	// setup_needed flips false after skip.
	checker := NewReadinessChecker(db, &config.Config{})
	assert.False(t, checker.SetupNeeded(t.Context()))
}

func TestSetupApply_TimezoneHotApplyFallsBackOnInvalid(t *testing.T) {
	// Valid names only reach SetStatsTimezone; the handler rejects invalid
	// ones earlier — verify the guard holds (stats timezone unchanged).
	defer SetStatsTimezone("")
	SetStatsTimezone("")
	db := setupReadinessTestDB(t)
	h := newSetupHandlerForTest(t, db, &config.Config{})
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/setup/apply", map[string]any{"timezone": "Mars/Olympus", "status": "done"})
	h.Apply(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "", statsTZName)
}
