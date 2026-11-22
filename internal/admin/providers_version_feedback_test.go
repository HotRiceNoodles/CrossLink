package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M0/M2 acceptance gaps: pre-persist validation rejects invalid api_version
// BEFORE the write (repo untouched), and the applied/apply_error fields tell
// "saved but not applied" apart from live config.

func TestProviderHandler_Create_InvalidAPIVersionRejectedBeforeWrite(t *testing.T) {
	t.Parallel()
	repo, modelRepo := defaultProviderMocks()
	created := false
	repo.createFn = func(ctx context.Context, p *model.Provider) error {
		created = true
		return nil
	}
	h := newProviderHandler(repo, modelRepo)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/providers", gin.H{
		"name": "p1", "display_name": "p1", "adapter_type": "anthropic",
		"base_url": "https://api.openai.com/v1", "api_key": "k",
		"extra_config": map[string]any{"api_version": "2024-01-01"},
	})
	h.Create(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "unsupported anthropic api_version")
	assert.False(t, created, "invalid config must fail BEFORE the row is persisted")
}

func TestProviderHandler_Update_InvalidAPIVersionRejected(t *testing.T) {
	t.Parallel()
	repo, modelRepo := defaultProviderMocks()
	updated := false
	repo.updateFn = func(ctx context.Context, p *model.Provider) error {
		updated = true
		return nil
	}
	h := newProviderHandler(repo, modelRepo)

	c, w := newTestContext(t, http.MethodPut, "/admin/api/providers/7", gin.H{
		"extra_config": map[string]any{"api_version": "latest"},
	})
	c.Params = gin.Params{{Key: "id", Value: "7"}}
	h.Update(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.False(t, updated, "invalid incoming extra_config must fail before save")
}

func TestProviderHandler_AppliedFeedback(t *testing.T) {
	t.Parallel()

	// Constructible adapter → applied=true, no apply_error.
	repo, modelRepo := defaultProviderMocks()
	repo.createFn = func(ctx context.Context, p *model.Provider) error { return nil }
	h := newProviderHandler(repo, modelRepo)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/providers", gin.H{
		"name": "ok-prov", "display_name": "ok", "adapter_type": "anthropic",
		"base_url": "https://api.openai.com/v1", "api_key": "k",
		"extra_config": map[string]any{"api_version": "2026-10-21"},
	})
	h.Create(c)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Contains(t, w.Body.String(), `"applied":true`)
	assert.NotContains(t, w.Body.String(), "apply_error")

	// Unknown adapter type persists (no adapter-type validation by design)
	// but cannot be applied to the runtime registry — the response must say
	// saved-but-not-applied instead of masquerading as live.
	repo2, modelRepo2 := defaultProviderMocks()
	repo2.createFn = func(ctx context.Context, p *model.Provider) error { return nil }
	h2 := newProviderHandler(repo2, modelRepo2)
	c2, w2 := newTestContext(t, http.MethodPost, "/admin/api/providers", gin.H{
		"name": "bad-prov", "display_name": "bad", "adapter_type": "no-such-adapter",
		"base_url": "https://api.openai.com/v1", "api_key": "k",
	})
	h2.Create(c2)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Contains(t, w2.Body.String(), `"applied":false`)
	assert.Contains(t, w2.Body.String(), "apply_error")
}
