package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/settings"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestOnboarding_RoutesNoPanic confirms Gin accepts the literal /providers/probe
// route alongside /providers/:id/* without panicking at registration time.
func TestOnboarding_RoutesNoPanic(t *testing.T) {
	r := gin.New()
	noop := func(c *gin.Context) {}
	r.POST("/providers", noop)
	r.POST("/providers/probe", noop)
	r.POST("/providers/:id/test", noop)
	r.GET("/providers/:id/models", noop)
	r.PUT("/providers/:id", noop)
	r.DELETE("/providers/:id", noop)
	// Reaching here means no panic.
	assert.NotNil(t, r)
}

func setupOnboardingTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Provider{}, &model.ProviderModel{}, &model.APIKey{}, &model.APIKeyHash{}, &model.SystemSetting{}))
	return db
}

// stubAuthorizer is a fixed roleID→actions table for wizard authorization tests.
type stubAuthorizer struct{ roles map[int64]map[string]bool }

func (s stubAuthorizer) HasPermission(roleID int64, action string) bool {
	return s.roles[roleID][action]
}

// allowAllAuthorizer grants everything (happy-path helpers test schema/flow,
// not authorization — role IDs there are arbitrary).
type allowAllAuthorizer struct{}

func (allowAllAuthorizer) HasPermission(int64, string) bool { return true }

// allActions mirrors the seeded admin role for the authz tests.
var allActions = []string{
	"provider:list", "provider:create", "provider:update", "provider:delete", "provider:test",
	"model:list", "model:create", "model:update", "model:delete",
	"key:list", "key:create", "key:update", "key:delete", "key:regenerate",
}

func newOnboardingHandlerForTest(db *gorm.DB) *OnboardingHandler {
	cp, _ := crypto.NewProvider("standard")
	h := NewOnboardingHandler(db, nil, cp, nil, provider.NewRegistry(), nil, nil, nil)
	h.SetAuthorizer(allowAllAuthorizer{})
	return h
}

func actionsSet(actions []string) map[string]bool {
	m := make(map[string]bool, len(actions))
	for _, a := range actions {
		m[a] = true
	}
	return m
}

// --- Probe: SSRF rejection (no outbound HTTP required) ---

func TestOnboarding_Probe_SSRFRejected(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/v1",
		"http://169.254.169.254/v1",
		"http://10.0.0.1/v1",
		"http://192.168.1.1/v1",
	}
	for _, baseURL := range cases {
		t.Run(baseURL, func(t *testing.T) {
			h := newOnboardingHandlerForTest(setupOnboardingTestDB(t))
			c, w := newTestContext(t, http.MethodPost, "/admin/api/providers/probe", gin.H{
				"adapter_type": "openai_compatible",
				"base_url":     baseURL,
				"api_key":      "sk-test",
			})
			setAdminContext(c, 1, 1, "admin")
			h.Probe(c)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			var resp map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, "provider_url_invalid", resp["error_code"])
		})
	}
}

func TestOnboarding_Probe_InvalidURL(t *testing.T) {
	h := newOnboardingHandlerForTest(setupOnboardingTestDB(t))
	c, w := newTestContext(t, http.MethodPost, "/admin/api/providers/probe", gin.H{
		"adapter_type": "openai_compatible",
		"base_url":     "ftp://example.com",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Probe(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestOnboarding_Probe_UnsupportedAdapter(t *testing.T) {
	h := newOnboardingHandlerForTest(setupOnboardingTestDB(t))
	// No base_url → SSRF check skipped, so we isolate the adapter-lookup path.
	c, w := newTestContext(t, http.MethodPost, "/admin/api/providers/probe", gin.H{
		"adapter_type": "nonexistent_adapter",
		"api_key":      "sk-test",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Probe(c)
	// Returns 200 with success:false — probe failures are not HTTP errors.
	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Data struct {
			Success         bool   `json:"success"`
			ModelsSupported bool   `json:"models_supported"`
			Error           string `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.Data.Success)
	assert.False(t, resp.Data.ModelsSupported)
	assert.NotEmpty(t, resp.Data.Error)
}

// --- Commit: happy path ---

func TestOnboarding_Commit_HappyPath(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", gin.H{
		"provider": gin.H{
			"name": "openai", "display_name": "OpenAI", "adapter_type": "openai_compatible",
			"base_url": "https://api.openai.com/v1", "api_key": "sk-test",
		},
		"models": []gin.H{
			{"model_name": "gpt-4o", "provider_model": "gpt-4o"},
			{"model_name": "gpt-4o-mini", "provider_model": "gpt-4o-mini"},
		},
		"currency": "CNY",
		"key":      gin.H{"name": "onboarding-key"},
	})
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Data struct {
			ProviderID int64   `json:"provider_id"`
			ModelIDs   []int64 `json:"model_ids"`
			Key        string  `json:"key"`
			KeyPrefix  string  `json:"key_prefix"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Greater(t, resp.Data.ProviderID, int64(0))
	assert.Len(t, resp.Data.ModelIDs, 2)
	assert.NotEmpty(t, resp.Data.Key)
	assert.Equal(t, "cl-", resp.Data.Key[:3])
	assert.Equal(t, resp.Data.Key[:7], resp.Data.KeyPrefix)

	// All three tables populated.
	var p model.Provider
	require.NoError(t, db.First(&p, resp.Data.ProviderID).Error)
	assert.Equal(t, "openai", p.Name)

	var modelCount int64
	db.Model(&model.ProviderModel{}).Where("provider_id = ?", p.ID).Count(&modelCount)
	assert.EqualValues(t, 2, modelCount)

	var keyCount int64
	db.Model(&model.APIKey{}).Where("name = ?", "onboarding-key").Count(&keyCount)
	assert.EqualValues(t, 1, keyCount)

	// Hash record present with the key.
	var hashCount int64
	db.Model(&model.APIKeyHash{}).Count(&hashCount)
	assert.EqualValues(t, 1, hashCount)
}

// --- Commit: billing currency (write-once platform selection) ---

// A first commit without a currency must be rejected — no silent default on a
// fresh install — and must leave no rows behind.
func TestOnboarding_Commit_CurrencyRequiredOnFirstCommit(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)

	body := wizardBody("no-currency")
	delete(body, "currency")
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "billing currency")
	var providerCount, settingCount int64
	db.Model(&model.Provider{}).Count(&providerCount)
	db.Model(&model.SystemSetting{}).Count(&settingCount)
	assert.EqualValues(t, 0, providerCount)
	assert.EqualValues(t, 0, settingCount)
}

// A malformed currency value is rejected before any write.
func TestOnboarding_Commit_CurrencyInvalid(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)

	body := wizardBody("bad-currency")
	body["currency"] = "EURO"
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "ISO 4217")
	var providerCount, settingCount int64
	db.Model(&model.Provider{}).Count(&providerCount)
	db.Model(&model.SystemSetting{}).Count(&settingCount)
	assert.EqualValues(t, 0, providerCount)
	assert.EqualValues(t, 0, settingCount)
}

// The currency is the operator's choice: any well-formed ISO 4217 code is
// accepted, stored canonically, echoed, and synced to the process-global
// currency that every billing row is stamped with.
func TestOnboarding_Commit_AnyCurrency(t *testing.T) {
	t.Cleanup(func() { settings.SetPlatformCurrency(settings.CurrencyCNY) })
	for _, tc := range []struct{ sent, want string }{
		{"EUR", "EUR"}, {"JPY", "JPY"}, {"GBP", "GBP"}, {"HKD", "HKD"}, {"TWD", "TWD"}, {"KRW", "KRW"},
		{"eur", "EUR"}, {" jpy ", "JPY"}, // a lower-case spelling is the same code
	} {
		t.Run(tc.sent, func(t *testing.T) {
			db := setupOnboardingTestDB(t)
			h := newOnboardingHandlerForTest(db)
			body := wizardBody("any-currency")
			body["currency"] = tc.sent
			c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
			setAdminContext(c, 1, 1, "admin")
			h.Commit(c)
			require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

			var resp struct {
				Data struct {
					BillingCurrency string `json:"billing_currency"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, tc.want, resp.Data.BillingCurrency)

			var row model.SystemSetting
			require.NoError(t, db.Where("system_settings.key = ?", "billing_currency").First(&row).Error)
			assert.Equal(t, tc.want, row.Value, "stored canonically")
			assert.Equal(t, tc.want, settings.PlatformCurrency(), "process-global currency follows the choice")
		})
	}
}

// A later commit spelling the locked currency differently is the SAME value
// (idempotent), not an attempt to change it.
func TestOnboarding_Commit_LockedCurrencyCaseInsensitive(t *testing.T) {
	t.Cleanup(func() { settings.SetPlatformCurrency(settings.CurrencyCNY) })
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", wizardBody("first"))
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String()) // wizardBody fixes CNY

	body := wizardBody("second")
	body["currency"] = "cny"
	c2, w2 := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
	setAdminContext(c2, 2, 1, "admin")
	h.Commit(c2)
	require.Equal(t, http.StatusCreated, w2.Code, "body: %s", w2.Body.String())
}

// The first commit fixes billing_currency; later commits may repeat it or omit
// it, but a DIFFERENT value is a 409 and creates nothing.
func TestOnboarding_Commit_CurrencyLockedAfterFirstCommit(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", wizardBody("first"))
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var first struct {
		Data struct {
			BillingCurrency string `json:"billing_currency"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
	assert.Equal(t, "CNY", first.Data.BillingCurrency)

	var row model.SystemSetting
	require.NoError(t, db.Where("system_settings.key = ?", "billing_currency").First(&row).Error)
	assert.Equal(t, "CNY", row.Value)

	// Same value → idempotent success.
	c2, w2 := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", wizardBody("second"))
	setAdminContext(c2, 2, 1, "admin")
	h.Commit(c2)
	require.Equal(t, http.StatusCreated, w2.Code, "body: %s", w2.Body.String())

	// Different value → 409, nothing created for that request.
	body := wizardBody("third")
	body["currency"] = "USD"
	c3, w3 := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
	setAdminContext(c3, 3, 1, "admin")
	h.Commit(c3)
	require.Equal(t, http.StatusConflict, w3.Code, "body: %s", w3.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w3.Body.Bytes(), &resp))
	assert.Equal(t, ErrBillingCurrencyLocked, resp["error_code"])

	var providerCount int64
	db.Model(&model.Provider{}).Where("name = ?", "third").Count(&providerCount)
	assert.EqualValues(t, 0, providerCount, "locked-currency commit must create nothing")
	require.NoError(t, db.Where("system_settings.key = ?", "billing_currency").First(&row).Error)
	assert.Equal(t, "CNY", row.Value, "currency row unchanged")
}

// TestOnboarding_Commit_NoKey: 向导不再强制建首密钥（生成的明文从不展示，被动创建属
// 无意义副作用）——不带 key 时事务只建 供应商+映射，APIKey/APIKeyHash 零行，
// 响应不含 key/key_prefix 字段。
func TestOnboarding_Commit_NoKey(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", gin.H{
		"provider": gin.H{
			"name": "nokey-provider", "display_name": "NoKey", "adapter_type": "openai_compatible",
			"base_url": "https://api.openai.com/v1", "api_key": "sk-test",
		},
		"models":   []gin.H{{"model_name": "gpt-4o", "provider_model": "gpt-4o"}},
		"currency": "CNY",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Data struct {
			ProviderID int64  `json:"provider_id"`
			Key        string `json:"key"`
			KeyPrefix  string `json:"key_prefix"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Greater(t, resp.Data.ProviderID, int64(0))
	assert.Empty(t, resp.Data.Key, "no key material in response when key step omitted")
	assert.Empty(t, resp.Data.KeyPrefix)

	var keyCount, hashCount int64
	db.Model(&model.APIKey{}).Count(&keyCount)
	db.Model(&model.APIKeyHash{}).Count(&hashCount)
	assert.EqualValues(t, 0, keyCount, "no APIKey rows should be created")
	assert.EqualValues(t, 0, hashCount, "no APIKeyHash rows should be created")

	var modelCount int64
	db.Model(&model.ProviderModel{}).Where("provider_id = ?", resp.Data.ProviderID).Count(&modelCount)
	assert.EqualValues(t, 1, modelCount)
}

// --- Commit: transaction rollback (core guarantee) ---

func TestOnboarding_Commit_TransactionRollback(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)

	// model_name has size:128 in the GORM tag but SQLite doesn't enforce size.
	// Force a real failure by submitting an empty provider name (NOT NULL violation
	// via the binding) — instead we pre-seed a provider name to trigger uniqueIndex.
	require.NoError(t, db.Create(&model.Provider{Name: "taken", DisplayName: "x", AdapterType: "openai_compatible", BaseURL: "https://x", Status: 1}).Error)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", gin.H{
		"provider": gin.H{
			"name": "taken", "display_name": "Dup", "adapter_type": "openai_compatible",
			"base_url": "https://api.openai.com/v1", "api_key": "sk-test",
		},
		"models":   []gin.H{{"model_name": "m1", "provider_model": "m1"}},
		"currency": "CNY",
		"key":      gin.H{"name": "k1"},
	})
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)

	// Duplicate name → 409 conflict.
	assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())

	// No partial state: only the pre-seeded provider exists, no models, no keys.
	var providerCount int64
	db.Model(&model.Provider{}).Count(&providerCount)
	assert.EqualValues(t, 1, providerCount, "no new provider should be created")

	var modelCount int64
	db.Model(&model.ProviderModel{}).Count(&modelCount)
	assert.EqualValues(t, 0, modelCount, "no models should leak on rollback")

	var keyCount int64
	db.Model(&model.APIKey{}).Count(&keyCount)
	assert.EqualValues(t, 0, keyCount, "no keys should leak on rollback")
}

// --- Commit: existing-provider reuse (B2+C root-cause fix) ---

// When a provider with the same name already exists (e.g. from the seed file)
// and reuse_provider is NOT set, Commit must return a structured 409 with the
// existing provider's id/name so the wizard can prompt "reuse or rename".
func TestOnboarding_Commit_ExistingProviderReturnsStructured409(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)
	// Pre-seed an existing provider (simulates configs/providers.yaml).
	require.NoError(t, db.Create(&model.Provider{
		Name: "deepseek", DisplayName: "DeepSeek", AdapterType: "openai_compatible",
		BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-existing", Status: 1,
	}).Error)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", gin.H{
		"provider": gin.H{"name": "deepseek", "display_name": "DeepSeek", "adapter_type": "openai_compatible", "base_url": "https://api.deepseek.com/v1", "api_key": "sk-new"},
		"models":   []gin.H{{"model_name": "deepseek-chat", "provider_model": "deepseek-chat"}},
		"currency": "CNY",
		"key":      gin.H{"name": "k"},
	})
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)

	assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, ErrProviderExists, resp["error_code"])
	assert.NotZero(t, resp["existing_provider_id"])
	assert.Equal(t, "deepseek", resp["existing_provider_name"])

	// No new provider / key created.
	var keyCount int64
	db.Model(&model.APIKey{}).Count(&keyCount)
	assert.EqualValues(t, 0, keyCount)
}

// With reuse_provider=true, Commit reuses the existing provider (no new row),
// skips models that already exist under it, creates missing models + the key.
func TestOnboarding_Commit_ReuseProviderCreatesKeyOnly(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)
	require.NoError(t, db.Create(&model.Provider{
		Name: "deepseek", DisplayName: "DeepSeek", AdapterType: "openai_compatible",
		BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-existing", Status: 1,
	}).Error)
	var pid int64
	db.Model(&model.Provider{}).Where("name = ?", "deepseek").Pluck("id", &pid)
	// An existing model under deepseek (should be skipped, not duplicated).
	require.NoError(t, db.Create(&model.ProviderModel{
		ProviderID: pid, ModelName: "deepseek-chat", ProviderModel: "deepseek-chat",
		RoutingStrategy: "weighted_random", Status: 1,
	}).Error)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", gin.H{
		"provider":       gin.H{"name": "deepseek", "display_name": "DeepSeek", "adapter_type": "openai_compatible", "base_url": "https://api.deepseek.com/v1", "api_key": "sk-new"},
		"models":         []gin.H{{"model_name": "deepseek-chat", "provider_model": "deepseek-chat"}, {"model_name": "deepseek-reasoner", "provider_model": "deepseek-reasoner"}},
		"currency":       "CNY",
		"key":            gin.H{"name": "reused-key"},
		"reuse_provider": true,
	})
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Data struct {
			ProviderID     int64  `json:"provider_id"`
			Key            string `json:"key"`
			ReusedProvider bool   `json:"reused_provider"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Data.ReusedProvider, "reused_provider flag must be true")
	assert.Equal(t, pid, resp.Data.ProviderID, "must reuse existing provider id")
	assert.NotEmpty(t, resp.Data.Key)

	// No new provider row, exactly one key, and the pre-existing model not duplicated.
	var providerCount, keyCount, modelCount int64
	db.Model(&model.Provider{}).Where("name = ?", "deepseek").Count(&providerCount)
	assert.EqualValues(t, 1, providerCount)
	db.Model(&model.APIKey{}).Count(&keyCount)
	assert.EqualValues(t, 1, keyCount)
	db.Model(&model.ProviderModel{}).Where("provider_id = ?", pid).Count(&modelCount)
	assert.EqualValues(t, 2, modelCount, "1 pre-existing + 1 newly added (deepseek-reasoner)")
}

// TestIsDuplicateNameErr_Locales guards the duplicate-name detection against
// Postgres locale variants. Production reported a zh_CN regression where
// "重复键违反唯一约束" was not recognized and the handler returned 500 instead
// of 409 — SQLSTATE 23505 (locale-independent) and the Chinese keyword close it.
func TestIsDuplicateNameErr_Locales(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf(`pq: duplicate key value violates unique constraint "providers_name_key"`), true},
		{fmt.Errorf(`create provider: 错误: 重复键违反唯一约束"providers_name_key" (SQLSTATE 23505)`), true},
		{fmt.Errorf(`重复键违反唯一约束`), true},
		{fmt.Errorf("UNIQUE constraint failed: providers.name"), true},
		{fmt.Errorf("duplicate key"), true},
		{fmt.Errorf("connection refused"), false},
		{fmt.Errorf("invalid input syntax"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isDuplicateNameErr(c.err); got != c.want {
			t.Errorf("isDuplicateNameErr(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// --- Audit item 1: operation-level authorization + tenant scoping ---

// wizardBody is a canonical valid commit payload; per-case tweaks apply on top.
func wizardBody(providerName string) gin.H {
	return gin.H{
		"provider": gin.H{"name": providerName, "display_name": "X", "adapter_type": "openai_compatible",
			"base_url": "https://api.openai.com/v1", "api_key": "sk-test"},
		"models":   []gin.H{{"model_name": "m1", "provider_model": "m1"}},
		"currency": "CNY",
		"key":      gin.H{"name": "k1"},
	}
}

func newOnboardingHandlerWithAuthz(db *gorm.DB, roles map[int64]map[string]bool) *OnboardingHandler {
	cp, _ := crypto.NewProvider("standard")
	h := NewOnboardingHandler(db, nil, cp, nil, provider.NewRegistry(), nil, nil, nil)
	h.SetAuthorizer(stubAuthorizer{roles: roles})
	return h
}

// Read-only roles (viewer: provider/model/key list only) must not be able to
// write through the wizard even with a valid admin session.
func TestOnboarding_Commit_ViewerRoleForbidden(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerWithAuthz(db, map[int64]map[string]bool{
		5: {"provider:list": true, "model:list": true, "key:list": true},
	})
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", wizardBody("viewer-provider"))
	setAdminContext(c, 1, 5, "viewer")
	h.Commit(c)
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	var providerCount, modelCount, keyCount int64
	db.Model(&model.Provider{}).Count(&providerCount)
	db.Model(&model.ProviderModel{}).Count(&modelCount)
	db.Model(&model.APIKey{}).Count(&keyCount)
	assert.EqualValues(t, 0, providerCount)
	assert.EqualValues(t, 0, modelCount)
	assert.EqualValues(t, 0, keyCount)
}

// A role that may create providers+models but not keys can finish a keyless
// commit but must be denied when the request includes a key.
func TestOnboarding_Commit_KeylessAllowedWithoutKeyCreate(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerWithAuthz(db, map[int64]map[string]bool{
		7: {"provider:create": true, "model:create": true},
	})

	// Keyless commit succeeds.
	body := wizardBody("no-key-perm")
	delete(body, "key")
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
	setAdminContext(c, 1, 7, "member")
	h.Commit(c)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	// Same role WITH a key step → 403, and nothing from the second request leaks.
	c2, w2 := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", wizardBody("with-key"))
	setAdminContext(c2, 2, 7, "member")
	h.Commit(c2)
	assert.Equal(t, http.StatusForbidden, w2.Code, "body: %s", w2.Body.String())
	var keyCount int64
	db.Model(&model.APIKey{}).Count(&keyCount)
	assert.EqualValues(t, 0, keyCount)
}

// Reusing an existing provider needs model/key permissions but NOT
// provider:create — the provider row is not written in reuse mode.
func TestOnboarding_Commit_ReuseNeedsNoProviderCreate(t *testing.T) {
	db := setupOnboardingTestDB(t)
	org := int64(0)
	require.NoError(t, db.Create(&model.Provider{
		Name: "shared", DisplayName: "Shared", AdapterType: "openai_compatible",
		BaseURL: "https://api.openai.com/v1", APIKey: "sk-existing", Status: 1, OrgID: &org,
	}).Error)

	h := newOnboardingHandlerWithAuthz(db, map[int64]map[string]bool{
		7: {"model:create": true, "key:create": true},
	})
	body := wizardBody("shared")
	body["reuse_provider"] = true
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
	setAdminContext(c, 1, 7, "member")
	h.Commit(c)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var providerCount int64
	db.Model(&model.Provider{}).Count(&providerCount)
	assert.EqualValues(t, 1, providerCount, "reuse must not create a second provider row")
}

// nil authorizer (wiring bug) must fail CLOSED, not fall back to allow.
func TestOnboarding_Commit_NilAuthorizerDenied(t *testing.T) {
	db := setupOnboardingTestDB(t)
	cp, _ := crypto.NewProvider("standard")
	h := NewOnboardingHandler(db, nil, cp, nil, provider.NewRegistry(), nil, nil, nil) // no SetAuthorizer
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", wizardBody("wiring-bug"))
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// Cross-tenant reuse: an org-scoped caller must not attach mappings to a
// provider owned by another org, and the response must not disclose the other
// tenant's provider id/name (no structured 409 fields; no model rows created).
func TestOnboarding_Commit_ReuseCrossTenantDenied(t *testing.T) {
	db := setupOnboardingTestDB(t)
	otherOrg := int64(200)
	require.NoError(t, db.Create(&model.Provider{
		Name: "tenant-provider", DisplayName: "Other", AdapterType: "openai_compatible",
		BaseURL: "https://api.openai.com/v1", APIKey: "sk-other", Status: 1, OrgID: &otherOrg,
	}).Error)

	h := newOnboardingHandlerWithAuthz(db, map[int64]map[string]bool{
		7: actionsSet(allActions),
	})
	body := wizardBody("tenant-provider")
	body["reuse_provider"] = true
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
	setAdminContext(c, 1, 7, "member")
	c.Set("org_id", int64(100))
	h.Commit(c)

	// The org-scoped pre-check does not see the other tenant's provider → falls
	// through to create → global unique name → plain 409 WITHOUT disclosure.
	assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotContains(t, resp, "existing_provider_id", "cross-tenant provider id must not be disclosed")
	assert.NotContains(t, resp, "existing_provider_name", "cross-tenant provider name must not be disclosed")

	var modelCount, keyCount int64
	db.Model(&model.ProviderModel{}).Count(&modelCount)
	db.Model(&model.APIKey{}).Count(&keyCount)
	assert.EqualValues(t, 0, modelCount, "no mappings may be attached to another tenant's provider")
	assert.EqualValues(t, 0, keyCount)
}

// Same-tenant reuse still works for an org-scoped caller (regression guard
// for the scoping change itself).
func TestOnboarding_Commit_ReuseSameTenantAllowed(t *testing.T) {
	db := setupOnboardingTestDB(t)
	org := int64(100)
	require.NoError(t, db.Create(&model.Provider{
		Name: "own-provider", DisplayName: "Own", AdapterType: "openai_compatible",
		BaseURL: "https://api.openai.com/v1", APIKey: "sk-own", Status: 1, OrgID: &org,
	}).Error)

	h := newOnboardingHandlerWithAuthz(db, map[int64]map[string]bool{
		7: {"model:create": true, "key:create": true},
	})
	body := wizardBody("own-provider")
	body["reuse_provider"] = true
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
	setAdminContext(c, 1, 7, "member")
	c.Set("org_id", org)
	h.Commit(c)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var modelCount int64
	db.Model(&model.ProviderModel{}).Count(&modelCount)
	assert.EqualValues(t, 1, modelCount)
}

// Non-reuse duplicate under an org scope: same-org duplicates keep the
// structured 409 (wizard UX), so scoping must not break that flow.
func TestOnboarding_Commit_DuplicateSameOrgKeepsStructured409(t *testing.T) {
	db := setupOnboardingTestDB(t)
	org := int64(100)
	require.NoError(t, db.Create(&model.Provider{
		Name: "dup", DisplayName: "Dup", AdapterType: "openai_compatible",
		BaseURL: "https://api.openai.com/v1", APIKey: "sk-x", Status: 1, OrgID: &org,
	}).Error)

	h := newOnboardingHandlerWithAuthz(db, map[int64]map[string]bool{1: actionsSet(allActions)})
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", wizardBody("dup"))
	setAdminContext(c, 1, 1, "admin")
	c.Set("org_id", org)
	h.Commit(c)
	assert.Equal(t, http.StatusConflict, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "dup", resp["existing_provider_name"], "same-org duplicate keeps the reuse prompt UX")
}

// --- Acceptance boundary 2 (items 1/2) ---

// Degraded permission data (JWT middleware could not resolve the caller's
// role/status) must deny the composite writer — same posture as RequireAction.
func TestOnboarding_Commit_AuthPermDegradedDenied(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerForTest(db)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", wizardBody("degraded"))
	setAdminContext(c, 1, 1, "admin")
	middleware.MarkAuthPermDegraded(c)
	h.Commit(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "auth_degraded")
	var providerCount int64
	db.Model(&model.Provider{}).Count(&providerCount)
	assert.EqualValues(t, 0, providerCount)
}

// A caller WITHOUT provider:create must get 403 (not the structured 409 that
// discloses existing_provider_id/name) when the name collides — the permission
// gate runs BEFORE the existence pre-check.
func TestOnboarding_Commit_NoPermissionNoConflictDisclosure(t *testing.T) {
	db := setupOnboardingTestDB(t)
	require.NoError(t, db.Create(&model.Provider{
		Name: "taken-name", DisplayName: "X", AdapterType: "openai_compatible",
		BaseURL: "https://api.openai.com/v1", APIKey: "sk-x", Status: 1,
	}).Error)

	// Role holds model/key create but NOT provider:create.
	h := newOnboardingHandlerWithAuthz(db, map[int64]map[string]bool{
		7: {"model:create": true, "key:create": true},
	})
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", wizardBody("taken-name"))
	setAdminContext(c, 1, 7, "member")
	h.Commit(c)

	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "existing_provider_id", "permission denial must not disclose the provider id")
	assert.NotContains(t, w.Body.String(), "existing_provider_name")
	assert.Contains(t, w.Body.String(), "provider:create")
}

// Reuse requested but the name is FREE in scope → a new provider would be
// written → provider:create is enforced at that point (before any write).
func TestOnboarding_Commit_ReuseFreeNameStillNeedsProviderCreate(t *testing.T) {
	db := setupOnboardingTestDB(t)
	h := newOnboardingHandlerWithAuthz(db, map[int64]map[string]bool{
		7: {"model:create": true, "key:create": true},
	})
	body := wizardBody("brand-new-name")
	body["reuse_provider"] = true
	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", body)
	setAdminContext(c, 1, 7, "member")
	h.Commit(c)

	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "provider:create")
	var providerCount int64
	db.Model(&model.Provider{}).Count(&providerCount)
	assert.EqualValues(t, 0, providerCount)
}

// --- Commit: validation rejections ---
func TestOnboarding_Commit_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		body gin.H
		want int
	}{
		{"missing provider name", gin.H{
			"provider": gin.H{"display_name": "x", "adapter_type": "openai_compatible"},
			"key":      gin.H{"name": "k"},
		}, http.StatusBadRequest},
		{"ssrf base_url", gin.H{
			"provider": gin.H{"name": "p", "display_name": "x", "adapter_type": "openai_compatible", "base_url": "http://127.0.0.1"},
			"key":      gin.H{"name": "k"},
		}, http.StatusBadRequest},
		{"negative price", gin.H{
			"provider": gin.H{"name": "p", "display_name": "x", "adapter_type": "openai_compatible", "base_url": "https://api.openai.com/v1"},
			"models":   []gin.H{{"model_name": "m", "provider_model": "m", "input_price": -1}},
			"key":      gin.H{"name": "k"},
		}, http.StatusBadRequest},
		{"bad routing strategy", gin.H{
			"provider": gin.H{"name": "p", "display_name": "x", "adapter_type": "openai_compatible", "base_url": "https://api.openai.com/v1"},
			"models":   []gin.H{{"model_name": "m", "provider_model": "m", "routing_strategy": "bogus"}},
			"key":      gin.H{"name": "k"},
		}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newOnboardingHandlerForTest(setupOnboardingTestDB(t))
			c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", tc.body)
			setAdminContext(c, 1, 1, "admin")
			h.Commit(c)
			assert.Equal(t, tc.want, w.Code, "body: %s", w.Body.String())
		})
	}
}
