package admin

import (
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

// M4/P-8: model_name and provider_model are immutable on update — a rename
// attempt gets an explicit 400 with a stable error code; echoing the same
// value (or omitting the keys) stays compatible.

func newImmutableModelEnv(t *testing.T) (*ModelHandler, int64) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Provider{}, &model.ProviderModel{}))

	p := &model.Provider{Name: "p-imm", DisplayName: "p", AdapterType: "openai_compatible", BaseURL: "https://x", APIKey: "k", Status: 1}
	require.NoError(t, db.Create(p).Error)
	m := &model.ProviderModel{ProviderID: p.ID, ModelName: "m-old", ProviderModel: "up-old", Weight: 1, Priority: 1, Status: 1, RoutingStrategy: "weighted_random"}
	require.NoError(t, db.Create(m).Error)

	// FlushByModel hits Redis unconditionally (no disabled-mode guard), so the
	// handler test needs a real client — miniredis, not nil.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	h := NewModelHandler(
		repository.NewProviderModelCRUDRepo(db),
		&noopCacheInvalidator{},
		service.NewCacheService(rdb, db, config.CacheConfig{}), // disabled: zero cfg → no entries, but client is real
		nil, // auditSvc
	)
	return h, m.ID
}

func TestModelHandler_Update_ImmutableFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		body       gin.H
		wantStatus int
		wantCode   string
	}{
		{"rename model_name rejected", gin.H{"model_name": "m-new"}, http.StatusBadRequest, ErrModelImmutableFields},
		{"rename provider_model rejected", gin.H{"provider_model": "up-new"}, http.StatusBadRequest, ErrModelImmutableFields},
		{"echoing same values compatible", gin.H{"model_name": "m-old", "provider_model": "up-old", "weight": 2}, http.StatusOK, ""},
		{"omitting keys compatible", gin.H{"weight": 3}, http.StatusOK, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, id := newImmutableModelEnv(t)
			c, w := newTestContext(t, http.MethodPut, "/admin/api/models/"+strconv.FormatInt(id, 10), tc.body)
			c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
			h.Update(c)
			assert.Equal(t, tc.wantStatus, w.Code)
			if tc.wantCode != "" {
				assert.Contains(t, w.Body.String(), tc.wantCode)
			}
		})
	}
}
