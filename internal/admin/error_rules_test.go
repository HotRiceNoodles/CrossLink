package admin

import (
	"net/http"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	sqlite "github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupErrorRuleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ErrorClassificationRule{}))
	return db
}

func newErrorRuleHandler(db *gorm.DB) *ErrorRuleHandler {
	return NewErrorRuleHandler(repository.NewErrorRuleRepo(db), nil) // auditSvc nil (Community)
}

func TestErrorRule_CreateDefaultsAndList(t *testing.T) {
	h := newErrorRuleHandler(setupErrorRuleTestDB(t))

	c, w := newTestContext(t, http.MethodPost, "/admin/api/error-rules", gin.H{
		"match_field": "code",
		"pattern":     "insufficient_quota",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)
	require.Equal(t, http.StatusCreated, w.Code)

	var created struct {
		Data model.ErrorClassificationRule `json:"data"`
	}
	decodeResponse(t, w, &created)
	r := created.Data
	assert.NotZero(t, r.ID)
	assert.Equal(t, "code", r.MatchField)
	assert.Equal(t, "insufficient_quota", r.Pattern)
	assert.Equal(t, "quota", r.Classification) // defaulted
	assert.Equal(t, "account", r.Scope)        // defaulted
	assert.Equal(t, 100, r.Priority)           // defaulted
	assert.True(t, r.Enabled)                  // defaulted on
	assert.Nil(t, r.ProviderType)              // global

	// List returns the rule.
	c, w = newTestContext(t, http.MethodGet, "/admin/api/error-rules", nil)
	setAdminContext(c, 1, 1, "admin")
	h.List(c)
	require.Equal(t, http.StatusOK, w.Code)
	var listed struct {
		Data []model.ErrorClassificationRule `json:"data"`
	}
	decodeResponse(t, w, &listed)
	require.Len(t, listed.Data, 1)
	assert.Equal(t, "insufficient_quota", listed.Data[0].Pattern)
}

func TestErrorRule_Update(t *testing.T) {
	h := newErrorRuleHandler(setupErrorRuleTestDB(t))
	require.NoError(t, h.repo.Create(nil, &model.ErrorClassificationRule{
		MatchField: "code", Pattern: "old", Classification: "quota", Scope: "account", Priority: 100, Enabled: true,
	}))

	c, w := newTestContext(t, http.MethodPut, "/admin/api/error-rules/1", gin.H{
		"pattern":  "insufficient_quota",
		"scope":    "model",
		"priority": 10,
		"enabled":  false,
	})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})
	h.Update(c)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var updated struct {
		Data model.ErrorClassificationRule `json:"data"`
	}
	decodeResponse(t, w, &updated)
	assert.Equal(t, "insufficient_quota", updated.Data.Pattern)
	assert.Equal(t, "model", updated.Data.Scope)
	assert.Equal(t, 10, updated.Data.Priority)
	assert.False(t, updated.Data.Enabled)
	assert.Equal(t, "code", updated.Data.MatchField) // untouched
}

func TestErrorRule_Delete(t *testing.T) {
	h := newErrorRuleHandler(setupErrorRuleTestDB(t))
	require.NoError(t, h.repo.Create(nil, &model.ErrorClassificationRule{
		MatchField: "code", Pattern: "x", Classification: "quota", Scope: "account", Priority: 100, Enabled: true,
	}))

	c, w := newTestContext(t, http.MethodDelete, "/admin/api/error-rules/1", nil)
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})
	h.Delete(c)
	require.Equal(t, http.StatusOK, w.Code)

	// List is now empty.
	c, w = newTestContext(t, http.MethodGet, "/admin/api/error-rules", nil)
	setAdminContext(c, 1, 1, "admin")
	h.List(c)
	var listed struct {
		Data []model.ErrorClassificationRule `json:"data"`
	}
	decodeResponse(t, w, &listed)
	assert.Empty(t, listed.Data)
}

func TestErrorRule_Create_InvalidMatchField(t *testing.T) {
	h := newErrorRuleHandler(setupErrorRuleTestDB(t))

	c, w := newTestContext(t, http.MethodPost, "/admin/api/error-rules", gin.H{
		"match_field": "bogus",
		"pattern":     "x",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestErrorRule_Create_InvalidScope(t *testing.T) {
	h := newErrorRuleHandler(setupErrorRuleTestDB(t))

	c, w := newTestContext(t, http.MethodPost, "/admin/api/error-rules", gin.H{
		"match_field": "code",
		"pattern":     "x",
		"scope":       "galaxy",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestErrorRule_Update_NotFound(t *testing.T) {
	h := newErrorRuleHandler(setupErrorRuleTestDB(t))

	c, w := newTestContext(t, http.MethodPut, "/admin/api/error-rules/999", gin.H{"pattern": "x"})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: "999"}})
	h.Update(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}
