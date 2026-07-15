package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupCatalogDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.PromptTemplate{}))
	return db
}

func TestTemplateCatalog_ListReturnsMetadataOnly(t *testing.T) {
	db := setupCatalogDB(t)
	require.NoError(t, db.Create(&model.PromptTemplate{
		Name: "summarize", Description: "Summarize text",
		SystemPrompt: "SECRET IP PROMPT — must not leak",
		FewShot:      datatypes.JSON([]byte(`[{"role":"user","content":"secret example"}]`)),
		VariablesSchema: datatypes.JSON([]byte(`[{"name":"lang","type":"string","required":true},{"name":"max_words","type":"number","required":false,"default":200}]`)),
		TargetFormat: "auto", Status: 1,
	}).Error)
	require.NoError(t, db.Create(&model.PromptTemplate{
		Name: "disabled-one", SystemPrompt: "x", TargetFormat: "auto", Status: 1,
	}).Error)
	db.Model(&model.PromptTemplate{}).Where("name = ?", "disabled-one").Update("status", 0)

	h := NewTemplateCatalogHandler(db)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/templates", nil)
	h.List(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 1, "only active templates, disabled excluded")
	entry := resp.Data[0]
	assert.Equal(t, "summarize", entry["name"])
	assert.Equal(t, "Summarize text", entry["description"])

	// Security: system_prompt content + few_shot must NOT be exposed (IP stays server-side).
	assert.NotContains(t, entry, "system_prompt", "system_prompt must not leak to consumers")
	assert.NotContains(t, entry, "few_shot", "few_shot must not leak")
	assert.NotContains(t, w.Body.String(), "SECRET IP PROMPT", "prompt content must not appear anywhere")

	// Variables schema surfaced for callers.
	vars, ok := entry["variables"].([]any)
	require.True(t, ok)
	require.Len(t, vars, 1) // only the required var; optional-with-default handled in impl
	first := vars[0].(map[string]any)
	assert.Equal(t, "lang", first["name"])

	// curl example present and references the template name.
	example, _ := entry["example"].(string)
	assert.Contains(t, example, "summarize")
	assert.Contains(t, example, "x_context")
}
