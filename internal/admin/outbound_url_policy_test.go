package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Handler-level policy tests for the six admin entry points that admit an
// outbound URL (provider create, provider update, setup apply, onboarding
// probe, onboarding commit, system config gateway_base_url). All six must
// behave identically: strict default rejects restricted targets; a private
// allowlist admits private targets; the red line never opens.

const allowlistTestPrivateURL = "http://10.99.0.7:11434/v1"

// noOpCache satisfies CacheInvalidator for handler tests.
type noOpCache struct{}

func (noOpCache) Invalidate() {}

func newProviderHandlerForTest(t *testing.T) *ProviderHandler {
	t.Helper()
	repo := &mockProviderRepo{
		getByIDFn: func(_ context.Context, _, _ int64) (*model.Provider, error) {
			return &model.Provider{ID: 7, Name: "p", DisplayName: "p", AdapterType: "openai_compatible", BaseURL: "https://api.openai.com/v1", Status: 1}, nil
		},
		createFn: func(_ context.Context, _ *model.Provider) error { return nil },
		updateFn: func(_ context.Context, _ *model.Provider) error { return nil },
	}
	modelRepo := &mockProviderModelRepo{
		countByProviderIDFn: func(_ context.Context, _ int64) (int64, error) { return 0, nil },
	}
	return NewProviderHandler(repo, modelRepo, noOpCache{}, provider.NewRegistry(), nil, nil, nil, nil, nil, nil)
}

func TestProviderCreate_URLPolicy(t *testing.T) {
	h := newProviderHandlerForTest(t)
	body := gin.H{
		"name": "onprem", "display_name": "On-Prem", "adapter_type": "openai_compatible",
		"base_url": allowlistTestPrivateURL, "api_key": "sk-x",
	}

	// Strict default: rejected.
	c, w := newTestContext(t, http.MethodPost, "/admin/api/providers", body)
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "internal address")

	// Allowlisted: admitted.
	withAllowlist(t, "10.99.0.0/16")
	c, w = newTestContext(t, http.MethodPost, "/admin/api/providers", body)
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)
	assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	// Red line never opens, even allowlisted wide.
	withAllowlist(t, "10.0.0.0/8", "192.168.0.0/16")
	c, w = newTestContext(t, http.MethodPost, "/admin/api/providers", gin.H{
		"name": "meta", "display_name": "m", "adapter_type": "openai_compatible",
		"base_url": "http://169.254.169.254/latest", "api_key": "sk-x",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "internal address")
}

func TestProviderUpdate_URLPolicy(t *testing.T) {
	h := newProviderHandlerForTest(t)
	body := gin.H{
		"name": "onprem", "display_name": "On-Prem", "adapter_type": "openai_compatible",
		"base_url": allowlistTestPrivateURL, "api_key": "sk-x",
	}

	c, w := newTestContext(t, http.MethodPut, "/admin/api/providers/7", body)
	c.Params = gin.Params{{Key: "id", Value: "7"}}
	setAdminContext(c, 1, 1, "admin")
	h.Update(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "internal address")

	withAllowlist(t, "10.99.0.0/16")
	c, w = newTestContext(t, http.MethodPut, "/admin/api/providers/7", body)
	c.Params = gin.Params{{Key: "id", Value: "7"}}
	setAdminContext(c, 1, 1, "admin")
	h.Update(c)
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

func TestSetupApply_URLPolicy(t *testing.T) {
	apply := func(t *testing.T) int {
		db := setupReadinessTestDB(t)
		h := newSetupHandlerForTest(t, db, &config.Config{})
		defer SetStatsTimezone("")
		c, w := newTestContext(t, http.MethodPost, "/admin/api/system/setup/apply", map[string]any{
			"timezone": "Asia/Shanghai",
			"base_url": allowlistTestPrivateURL,
			"status":   "done",
		})
		h.Apply(c)
		return w.Code
	}

	t.Run("strict default rejects", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, apply(t))
	})
	t.Run("allowlist admits", func(t *testing.T) {
		withAllowlist(t, "10.99.0.0/16")
		assert.Equal(t, http.StatusOK, apply(t))
	})
}

func TestOnboardingProbe_URLPolicy(t *testing.T) {
	probe := func(t *testing.T) int {
		h := newOnboardingHandlerForTest(setupOnboardingTestDB(t))
		c, w := newTestContext(t, http.MethodPost, "/admin/api/providers/probe", gin.H{
			"adapter_type": "openai_compatible",
			"base_url":     allowlistTestPrivateURL,
			"api_key":      "sk-test",
		})
		setAdminContext(c, 1, 1, "admin")
		h.Probe(c)
		return w.Code
	}

	t.Run("strict default rejects", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, probe(t))
	})
	t.Run("allowlist admits (probe proceeds)", func(t *testing.T) {
		withAllowlist(t, "10.99.0.0/16")
		// With the URL admitted the probe moves on to the (unreachable)
		// upstream — it must NOT be an SSRF 400 anymore.
		code := probe(t)
		assert.NotEqual(t, http.StatusBadRequest, code)
	})
}

func TestOnboardingCommit_URLPolicy(t *testing.T) {
	commit := func(t *testing.T) int {
		h := newOnboardingHandlerForTest(setupOnboardingTestDB(t))
		c, w := newTestContext(t, http.MethodPost, "/admin/api/system/onboarding", gin.H{
			"provider": gin.H{
				"name": "onprem", "display_name": "On-Prem", "adapter_type": "openai_compatible",
				"base_url": allowlistTestPrivateURL, "api_key": "sk-test",
			},
			"models": []gin.H{{"model_name": "m1", "provider_model": "m1"}},
			"key":    gin.H{"name": "k"},
		})
		setAdminContext(c, 1, 1, "admin")
		h.Commit(c)
		return w.Code
	}

	t.Run("strict default rejects", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, commit(t))
	})
	t.Run("allowlist admits", func(t *testing.T) {
		withAllowlist(t, "10.99.0.0/16")
		assert.Equal(t, http.StatusCreated, commit(t))
	})
}

func TestSystemConfigGatewayBaseURL_URLPolicy(t *testing.T) {
	update := func(t *testing.T) int {
		h, _ := newSystemConfigHandlerForTest(t)
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{
			"gateway_base_url": allowlistTestPrivateURL,
		})
		h.UpdateConfig(c)
		return w.Code
	}

	t.Run("strict default rejects", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, update(t))
	})
	t.Run("allowlist admits", func(t *testing.T) {
		withAllowlist(t, "10.99.0.0/16")
		assert.Equal(t, http.StatusOK, update(t))
	})
	t.Run("red line never admits", func(t *testing.T) {
		h, _ := newSystemConfigHandlerForTest(t)
		withAllowlist(t, "10.0.0.0/8", "192.168.0.0/16")
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/config", map[string]any{
			"gateway_base_url": "http://127.0.0.1:8080",
		})
		h.UpdateConfig(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "internal address")
	})
}
