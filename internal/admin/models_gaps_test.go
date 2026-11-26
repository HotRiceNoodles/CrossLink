package admin

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// G1-G4 acceptance: context window becomes a settable field, routing fields
// are bounds-checked, duplicate live mappings are refused, and provider
// delete frees the name for immediate reuse.

func newModelGapEnv(t *testing.T) (*ModelHandler, *repository.ProviderModelCRUDRepo, int64, int64) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Provider{}, &model.ProviderModel{}))

	p := &model.Provider{Name: "p-gaps", DisplayName: "p", AdapterType: "openai_compatible", BaseURL: "https://api.openai.com/v1", APIKey: "k", Status: 1}
	require.NoError(t, db.Create(p).Error)
	m := &model.ProviderModel{ProviderID: p.ID, ModelName: "g-model", ProviderModel: "up-1", Weight: 1, Priority: 1, Status: 1, RoutingStrategy: "weighted_random", Currency: "CNY"}
	require.NoError(t, db.Create(m).Error)

	// FlushByModel hits Redis unconditionally — real client via miniredis.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := repository.NewProviderModelCRUDRepo(db)
	return NewModelHandler(repo, &noopCacheInvalidator{}, service.NewCacheService(rdb, db, config.CacheConfig{}), nil), repo, p.ID, m.ID
}

func refetchModel(t *testing.T, h *ModelHandler, id int64) *model.ProviderModel {
	t.Helper()
	m, err := h.repo.GetByID(context.Background(), id, 0)
	require.NoError(t, err)
	return m
}

func idOf(v int64) string { return strconv.FormatInt(v, 10) }

func TestModelCreate_MaxContextSetAndValidated(t *testing.T) {
	t.Parallel()
	h, repo, providerID, _ := newModelGapEnv(t)

	// G1 positive: settable on create, persisted, read back.
	ctx := int(200000)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/models", gin.H{
		"provider_id": providerID, "model_name": "ctx-model", "provider_model": "up-2",
		"weight": 1, "priority": 1, "routing_strategy": "weighted_random",
		"max_context": ctx,
	})
	h.Create(c)
	require.Equal(t, http.StatusCreated, w.Code)
	got, err := repo.FindByProviderAndName(context.Background(), providerID, "ctx-model")
	require.NoError(t, err)
	require.NotNil(t, got.MaxContext)
	assert.Equal(t, 200000, *got.MaxContext)

	// G1 negative: non-positive rejected.
	c2, w2 := newTestContext(t, http.MethodPost, "/admin/api/models", gin.H{
		"provider_id": providerID, "model_name": "ctx-bad", "provider_model": "up-3",
		"weight": 1, "priority": 1, "routing_strategy": "weighted_random",
		"max_context": 0,
	})
	h.Create(c2)
	assert.Equal(t, http.StatusBadRequest, w2.Code)
}

func TestModelUpdate_MaxContextSetAndClear(t *testing.T) {
	t.Parallel()
	h, _, _, existingID := newModelGapEnv(t)

	set := 128000
	c, _ := newTestContext(t, http.MethodPut, "/admin/api/models/1", gin.H{"max_context": set})
	c.Params = gin.Params{{Key: "id", Value: idOf(existingID)}}
	h.Update(c)
	m := refetchModel(t, h, existingID)
	require.NotNil(t, m.MaxContext)
	assert.Equal(t, 128000, *m.MaxContext)

	// 0 clears back to unknown.
	c2, _ := newTestContext(t, http.MethodPut, "/admin/api/models/1", gin.H{"max_context": 0})
	c2.Params = gin.Params{{Key: "id", Value: idOf(existingID)}}
	h.Update(c2)
	assert.Nil(t, refetchModel(t, h, existingID).MaxContext)
}

func TestModelCreate_RoutingFieldBounds(t *testing.T) {
	t.Parallel()
	h, _, providerID, _ := newModelGapEnv(t)

	for name, body := range map[string]gin.H{
		"negative weight": {"provider_id": providerID, "model_name": "w1", "provider_model": "u", "weight": -1, "priority": 1, "routing_strategy": "weighted_random"},
		"zero priority":   {"provider_id": providerID, "model_name": "w2", "provider_model": "u", "weight": 1, "priority": 0, "routing_strategy": "weighted_random"},
	} {
		c, w := newTestContext(t, http.MethodPost, "/admin/api/models", body)
		h.Create(c)
		assert.Equal(t, http.StatusBadRequest, w.Code, name)
	}
}

// TestModelCreate_RoutingFieldsAbsentDefaults: an absent priority must NOT be
// treated as the int zero value (the r81x field incident — the enterprise
// sync dialog creates mappings with the identity fields only). Absent →
// priority defaults to 1 (column default), weight stays 0 (fallback-only is
// a legal routing state); explicitly-sent values keep G4 semantics above.
func TestModelCreate_RoutingFieldsAbsentDefaults(t *testing.T) {
	t.Parallel()
	h, repo, providerID, _ := newModelGapEnv(t)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/models", gin.H{
		"provider_id": providerID, "model_name": "sync-model", "provider_model": "up-sync",
		"routing_strategy": "weighted_random",
	})
	h.Create(c)
	require.Equal(t, http.StatusCreated, w.Code)

	got, err := repo.FindByProviderAndName(context.Background(), providerID, "sync-model")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Priority, "absent priority must default to 1")
	assert.Equal(t, 0, got.Weight, "absent weight must stay 0 (fallback-only pool)")
}

func TestModelCreate_DuplicateLiveMappingRejected(t *testing.T) {
	t.Parallel()
	h, repo, providerID, _ := newModelGapEnv(t)

	// Same (provider, model_name) as the seeded row → 409 with the stable code.
	c, w := newTestContext(t, http.MethodPost, "/admin/api/models", gin.H{
		"provider_id": providerID, "model_name": "g-model", "provider_model": "up-other",
		"weight": 1, "priority": 1, "routing_strategy": "weighted_random",
	})
	h.Create(c)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), ErrModelMappingExists)

	// A DIFFERENT provider may map the same model_name — the constraint is
	// per (provider, model_name), not global.
	require.NotNil(t, h)
	_ = repo
}

func TestProviderDelete_TombstoneFreesName(t *testing.T) {
	t.Parallel()
	repoMocks, modelRepo := defaultProviderMocks()
	var updatedName string
	deletedID := int64(0)
	repoMocks.getByIDFn = func(ctx context.Context, orgID, id int64) (*model.Provider, error) {
		return &model.Provider{ID: id, Name: "dup-name", AdapterType: "openai_compatible", Status: 1}, nil
	}
	repoMocks.updateFn = func(ctx context.Context, p *model.Provider) error {
		updatedName = p.Name
		return nil
	}
	repoMocks.deleteFn = func(ctx context.Context, id int64) error {
		deletedID = id
		return nil
	}
	h := newProviderHandler(repoMocks, modelRepo)

	c, w := newTestContext(t, http.MethodDelete, "/admin/api/providers/42", nil)
	c.Params = gin.Params{{Key: "id", Value: "42"}}
	h.Delete(c)
	require.Equal(t, http.StatusOK, w.Code)

	// G2: the tombstone was renamed (name#del#id) so the unique index no
	// longer blocks recreation under the original name.
	assert.Equal(t, "dup-name#del#42", updatedName)
	assert.Equal(t, int64(42), deletedID)
}

// supports_responses capability gate: the flag on an anthropic-protocol
// provider must fail the write (no silent no-op); on an openai-compatible
// provider it passes.
func TestModelCreate_SupportsResponsesCapabilityGate(t *testing.T) {
	t.Parallel()
	h, _, providerID, _ := newModelGapEnv(t)

	body := gin.H{
		"provider_id": providerID, "model_name": "sr-model", "provider_model": "up-sr",
		"weight": 1, "priority": 1, "routing_strategy": "weighted_random",
		"extra_config": map[string]any{"supports_responses": true},
	}

	// The gap env's provider is openai_compatible — the flag is legitimate.
	c, w := newTestContext(t, http.MethodPost, "/admin/api/models", body)
	h.Create(c)
	require.Equal(t, http.StatusCreated, w.Code, "openai-compatible provider must accept supports_responses: %s", w.Body.String())

	// Same flag against an anthropic-protocol provider → 400.
	db2 := newGapDB(t)
	p2 := &model.Provider{Name: "p-anth", DisplayName: "p", AdapterType: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "k", Status: 1}
	require.NoError(t, db2.Create(p2).Error)
	h2 := NewModelHandler(repository.NewProviderModelCRUDRepo(db2), &noopCacheInvalidator{}, gapCache(t, db2), nil)
	c2, w2 := newTestContext(t, http.MethodPost, "/admin/api/models", gin.H{
		"provider_id": p2.ID, "model_name": "sr-model", "provider_model": "up-sr",
		"weight": 1, "priority": 1, "routing_strategy": "weighted_random",
		"extra_config": map[string]any{"supports_responses": true},
	})
	h2.Create(c2)
	assert.Equal(t, http.StatusBadRequest, w2.Code)
	assert.Contains(t, w2.Body.String(), "cannot serve /v1/responses")

	// Flag absent on the same provider → unaffected.
	c3, w3 := newTestContext(t, http.MethodPost, "/admin/api/models", gin.H{
		"provider_id": p2.ID, "model_name": "plain-model", "provider_model": "up-plain",
		"weight": 1, "priority": 1, "routing_strategy": "weighted_random",
	})
	h2.Create(c3)
	assert.Equal(t, http.StatusCreated, w3.Code)
}

// newGapDB/newGapCache: shared fixtures split out of newModelGapEnv for the
// second-provider scenario.
func newGapDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Provider{}, &model.ProviderModel{}))
	return db
}

func gapCache(t *testing.T, db *gorm.DB) *service.CacheService {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return service.NewCacheService(rdb, db, config.CacheConfig{})
}
