package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
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
	require.NoError(t, db.AutoMigrate(&model.Provider{}, &model.ProviderModel{}, &model.APIKey{}, &model.APIKeyHash{}))
	return db
}

func newOnboardingHandlerForTest(db *gorm.DB) *OnboardingHandler {
	cp, _ := crypto.NewProvider("standard")
	return NewOnboardingHandler(db, nil, cp, nil, provider.NewRegistry(), nil, nil, nil)
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
		"key": gin.H{"name": "onboarding-key"},
	})
	setAdminContext(c, 1, 1, "admin")
	h.Commit(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Data struct {
			ProviderID int64  `json:"provider_id"`
			ModelIDs   []int64 `json:"model_ids"`
			Key        string `json:"key"`
			KeyPrefix  string `json:"key_prefix"`
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
		"models": []gin.H{{"model_name": "m1", "provider_model": "m1"}},
		"key":    gin.H{"name": "k1"},
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
