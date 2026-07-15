package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	sqlite "github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTemplateHandlerDB(t *testing.T) (*gorm.DB, *service.TemplateRegistry) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.PromptTemplate{}))
	return db, service.NewTemplateRegistry(db)
}

func TestTemplateHandler_CreatePreviewDelete(t *testing.T) {
	db, reg := setupTemplateHandlerDB(t)
	h := NewTemplateHandler(db, reg, nil, nil, nil) // sync/cache/audit nil for unit

	// Create
	c, w := newTestContext(t, http.MethodPost, "/admin/api/templates", gin.H{
		"name": "summarize",
		"system_prompt": "Summarize in {{lang}}, max {{n}} words.",
		"variables_schema": []gin.H{
			{"name": "lang", "trusted": true},
			{"name": "n", "type": "number", "trusted": true, "default": 100},
		},
		"target_format": "openai",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var created struct {
		Data model.PromptTemplate `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.Equal(t, "summarize", created.Data.Name)
	// version is hidden from API (json:"-") by design — not asserted here

	// Preview renders with sample variables (no upstream call).
	c2, w2 := newTestContext(t, http.MethodPost, "/admin/api/templates/"+itoa(created.Data.ID)+"/preview", gin.H{
		"variables": gin.H{"lang": "中文", "n": 200},
	})
	setAdminContext(c2, 1, 1, "admin")
	setPathParams(c2, gin.Params{{Key: "id", Value: itoa(created.Data.ID)}})
	h.Preview(c2)
	require.Equal(t, http.StatusOK, w2.Code, "body: %s", w2.Body.String())
	var prev struct {
		Data struct {
			SystemPrompt string `json:"system_prompt"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &prev))
	assert.Equal(t, "Summarize in 中文, max 200 words.", prev.Data.SystemPrompt)

	// Delete (soft) → registry invalidated, subsequent lookup fails.
	c3, w3 := newTestContext(t, http.MethodDelete, "/admin/api/templates/"+itoa(created.Data.ID), nil)
	setAdminContext(c3, 1, 1, "admin")
	setPathParams(c3, gin.Params{{Key: "id", Value: itoa(created.Data.ID)}})
	h.Delete(c3)
	assert.Equal(t, http.StatusOK, w3.Code, "body: %s", w3.Body.String())

	_, ok := reg.Get(c3.Request.Context(), "summarize")
	assert.False(t, ok, "deleted template must not resolve")
}

func TestTemplateHandler_PreviewUntrustedInSystem(t *testing.T) {
	db, reg := setupTemplateHandlerDB(t)
	h := NewTemplateHandler(db, reg, nil, nil, nil)
	require.NoError(t, db.Create(&model.PromptTemplate{
		Name: "bad", SystemPrompt: "{{user_input}}", TargetFormat: "auto", Status: 1,
	}).Error)
	var id int64
	db.Model(&model.PromptTemplate{}).Where("name = ?", "bad").Pluck("id", &id)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/templates/"+itoa(id)+"/preview", gin.H{
		"variables": gin.H{"user_input": "x"},
	})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "id", Value: itoa(id)}})
	h.Preview(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "untrusted_var_in_system")
}

func itoa(i int64) string {
	return jsonIntString(i)
}

func jsonIntString(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}
