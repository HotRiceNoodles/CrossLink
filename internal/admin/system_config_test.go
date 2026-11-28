package admin

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/settings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSystemConfigHandlerForTest(t *testing.T) (*SystemHandler, *settings.Provider) {
	t.Helper()
	db := setupSystemTestDB(t)
	cfg := &config.Config{}
	provider := settings.NewProvider(db, cfg, nil)
	h := NewSystemHandler(db, nil, cfg.Admin, nil, nil, nil, nil, nil)
	h.SetSettingsProvider(provider)
	return h, provider
}

func TestSystemConfig_GetDefaults(t *testing.T) {
	t.Parallel()
	h, _ := newSystemConfigHandlerForTest(t)
	c, w := newTestContext(t, http.MethodGet, "/admin/api/system/config", nil)
	h.GetConfig(c)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data struct {
			Settings map[string]any              `json:"settings"`
			Meta     map[string]settings.APIMeta `json:"meta"`
		} `json:"data"`
	}
	decodeResponse(t, w, &resp)
	assert.NotEmpty(t, resp.Data.Settings)
	assert.Len(t, resp.Data.Meta, len(settings.Registry))
	// Sensitive key never carries a value; meta flags it.
	assert.Equal(t, "", resp.Data.Settings[settings.KeySMTPPassword])
	assert.True(t, resp.Data.Meta[settings.KeySMTPPassword].Sensitive)
	assert.False(t, resp.Data.Meta[settings.KeySMTPPassword].Set)
	// Every registry key appears in both maps.
	for _, d := range settings.Registry {
		_, ok := resp.Data.Settings[d.Name]
		assert.True(t, ok, d.Name)
		_, ok = resp.Data.Meta[d.Name]
		assert.True(t, ok, d.Name)
	}
}

func TestSystemConfig_UpdateAppliesAndHotReloads(t *testing.T) {
	t.Parallel()
	h, provider := newSystemConfigHandlerForTest(t)
	db := h.db

	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{
		"smtp_host":      "smtp.example.com",
		"smtp_port":      465,
		"rate_limit_rpm": 600,
	})
	h.UpdateConfig(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Rows persisted.
	var row model.SystemSetting
	require.NoError(t, db.Where("key = ?", "smtp_host").First(&row).Error)
	assert.Equal(t, "smtp.example.com", row.Value)
	// Hot-applied: provider picked the new values up immediately.
	assert.Equal(t, "smtp.example.com", provider.SMTP().Host)
	assert.Equal(t, 465, provider.SMTP().Port)
	assert.Equal(t, 600, provider.RateLimit().RPM)
}

func TestSystemConfig_UpdateValidation(t *testing.T) {
	t.Parallel()
	h, _ := newSystemConfigHandlerForTest(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown key", map[string]any{"no_such_key": "x"}},
		{"port out of range", map[string]any{"smtp_port": 99999}},
		{"port wrong type", map[string]any{"smtp_port": "four-six-five"}},
		{"bad bool", map[string]any{"rate_limit_fail_closed": "yes"}},
		{"empty cors list", map[string]any{"cors_allowed_origins": []string{}}},
		{"bad cors origin", map[string]any{"cors_allowed_origins": []string{"not an origin"}}},
		{"bad datalens duration", map[string]any{"datalens_agg_interval": "fast"}},
		{"bad gateway base_url scheme", map[string]any{"gateway_base_url": "ftp://x"}},
	}
	for _, tc := range cases {
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config", tc.body)
		h.UpdateConfig(c)
		assert.Equal(t, http.StatusBadRequest, w.Code, tc.name+": "+w.Body.String())
	}
}

func TestSystemConfig_NullDeletesAndFallsBack(t *testing.T) {
	t.Parallel()
	h, provider := newSystemConfigHandlerForTest(t)
	db := h.db

	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"rate_limit_rpm": 600})
	h.UpdateConfig(c)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 600, provider.RateLimit().RPM)

	// JSON null deletes the row → yaml/env default returns.
	c2, w2 := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"rate_limit_rpm": nil})
	h.UpdateConfig(c2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	var row model.SystemSetting
	assert.Error(t, db.Where("key = ?", "rate_limit_rpm").First(&row).Error, "row deleted")
	assert.Equal(t, 0, provider.RateLimit().RPM, "baseline (unset) rpm")
	assert.Equal(t, settings.SourceDefault, provider.Get().Source(settings.KeyRateLimitRPM))
}

func TestSystemConfig_SensitiveWriteOnly(t *testing.T) {
	t.Parallel()
	h, provider := newSystemConfigHandlerForTest(t)
	db := h.db

	// Keyless provider: stored plaintext with the value effective.
	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"smtp_password": "mail-pass"})
	h.UpdateConfig(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var row model.SystemSetting
	require.NoError(t, db.Where("key = ?", "smtp_password").First(&row).Error)
	assert.Equal(t, "mail-pass", row.Value, "keyless mode stores plaintext (documented fallback)")
	assert.Equal(t, "mail-pass", provider.SMTP().Password)

	// GET never returns it; meta reports set=true.
	c2, w2 := newTestContext(t, http.MethodGet, "/admin/api/system/config", nil)
	h.GetConfig(c2)
	var resp struct {
		Data struct {
			Settings map[string]any              `json:"settings"`
			Meta     map[string]settings.APIMeta `json:"meta"`
		} `json:"data"`
	}
	decodeResponse(t, w2, &resp)
	assert.Equal(t, "", resp.Data.Settings["smtp_password"])
	assert.True(t, resp.Data.Meta["smtp_password"].Set)

	// Empty string clears (same as null).
	c3, w3 := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"smtp_password": ""})
	h.UpdateConfig(c3)
	require.Equal(t, http.StatusOK, w3.Code)
	assert.Error(t, db.Where("key = ?", "smtp_password").First(&row).Error)
	assert.Equal(t, "", provider.SMTP().Password)
}

func TestSystemConfig_RestartRequiredEcho(t *testing.T) {
	t.Parallel()
	h, _ := newSystemConfigHandlerForTest(t)
	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{
		"mcp_enabled":                 true,
		"mcp_max_servers":             9,
		"guardrail_alert_concurrency": 16,
	})
	h.UpdateConfig(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Applied    []string `json:"applied"`
		RestartReq []string `json:"restart_required_applied"`
	}
	decodeResponse(t, w, &resp)
	assert.Contains(t, resp.RestartReq, "mcp_enabled")
	assert.Contains(t, resp.RestartReq, "guardrail_alert_concurrency")
	assert.NotContains(t, resp.RestartReq, "mcp_max_servers", "hot key")
}

func TestSystemConfig_CORSSelfLockoutGuard(t *testing.T) {
	t.Parallel()
	h, provider := newSystemConfigHandlerForTest(t)

	// Caller's Origin not in the new list → refused.
	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config",
		map[string]any{"cors_allowed_origins": []string{"https://other.example.com"}})
	c.Request.Header.Set("Origin", "https://admin.example.com")
	h.UpdateConfig(c)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, settings.SourceDefault, provider.Get().Source(settings.KeyCORSOrigins))

	// Origin included → applied.
	c2, w2 := newTestContext(t, http.MethodPut, "/admin/api/system/config",
		map[string]any{"cors_allowed_origins": []string{"https://admin.example.com"}})
	c2.Request.Header.Set("Origin", "https://admin.example.com")
	h.UpdateConfig(c2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	assert.Equal(t, settings.SourceDB, provider.Get().Source(settings.KeyCORSOrigins))
}

func TestSystemConfig_ProviderMissingIsHandled(t *testing.T) {
	t.Parallel()
	db := setupSystemTestDB(t)
	h := NewSystemHandler(db, nil, config.AdminConfig{}, nil, nil, nil, nil, nil)

	c, w := newTestContext(t, http.MethodGet, "/admin/api/system/config", nil)
	h.GetConfig(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// billing_currency is write-once: the first PUT fixes it, later PUTs may only
// repeat the same value (idempotent) — a different value is 409 and null
// (delete) is refused outright.
func TestSystemConfig_BillingCurrencyWriteOnce(t *testing.T) {
	t.Parallel()
	h, provider := newSystemConfigHandlerForTest(t)
	db := h.db

	// First write fixes CNY.
	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": "CNY"})
	h.UpdateConfig(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var row model.SystemSetting
	require.NoError(t, db.Where("key = ?", "billing_currency").First(&row).Error)
	assert.Equal(t, "CNY", row.Value)
	assert.Equal(t, "CNY", provider.Get().BillingCurrency)

	// Same value → idempotent 200.
	c2, w2 := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": "CNY"})
	h.UpdateConfig(c2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())

	// Different value → 409, row unchanged.
	c3, w3 := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": "USD"})
	h.UpdateConfig(c3)
	require.Equal(t, http.StatusConflict, w3.Code, w3.Body.String())
	assert.Contains(t, w3.Body.String(), ErrBillingCurrencyLocked)
	require.NoError(t, db.Where("key = ?", "billing_currency").First(&row).Error)
	assert.Equal(t, "CNY", row.Value)

	// null (delete) → refused, row survives.
	c4, w4 := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": nil})
	h.UpdateConfig(c4)
	require.Equal(t, http.StatusBadRequest, w4.Code, w4.Body.String())
	var cnt int64
	db.Model(&model.SystemSetting{}).Where("key = ?", "billing_currency").Count(&cnt)
	assert.EqualValues(t, 1, cnt)

	// Malformed code → 400.
	c5, w5 := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": "EURO"})
	h.UpdateConfig(c5)
	require.Equal(t, http.StatusBadRequest, w5.Code, w5.Body.String())
}

// billing_currency takes any well-formed ISO 4217 code — the operator's
// choice, not a fixed list — stored canonically, and the canonical form is
// what later writes are compared against (write-once).
func TestSystemConfig_BillingCurrencyAnyCode(t *testing.T) {
	// Sequential, and restores the process-global currency: other tests in
	// the package read it (every billing row is stamped with it).
	t.Cleanup(func() { settings.SetPlatformCurrency(settings.CurrencyCNY) })
	for _, tc := range []struct{ sent, want string }{{"EUR", "EUR"}, {"JPY", "JPY"}, {"hkd", "HKD"}, {" brl ", "BRL"}} {
		t.Run(tc.sent, func(t *testing.T) {
			h, provider := newSystemConfigHandlerForTest(t)
			db := h.db

			c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": tc.sent})
			h.UpdateConfig(c)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			var row model.SystemSetting
			require.NoError(t, db.Where("key = ?", "billing_currency").First(&row).Error)
			assert.Equal(t, tc.want, row.Value, "stored canonically")
			assert.Equal(t, tc.want, provider.Get().BillingCurrency)

			// A different spelling of the same code is the same value.
			c2, w2 := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": strings.ToLower(tc.want)})
			h.UpdateConfig(c2)
			require.Equal(t, http.StatusOK, w2.Code, "re-PUT of the same code in another spelling must be idempotent: %s", w2.Body.String())

			// Another currency is a change → 409.
			c3, w3 := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": "USD"})
			if tc.want == "USD" {
				c3, w3 = newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{"billing_currency": "EUR"})
			}
			h.UpdateConfig(c3)
			require.Equal(t, http.StatusConflict, w3.Code, w3.Body.String())
		})
	}
}
