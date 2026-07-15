package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	sqlite "github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupAssemblerRegistry(t *testing.T, templates ...*model.PromptTemplate) (*service.TemplateRegistry, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.PromptTemplate{}))
	for _, tpl := range templates {
		require.NoError(t, db.Create(tpl).Error)
	}
	return service.NewTemplateRegistry(db), db
}

// runAssembler builds a gin context with the given JSON body already buffered
// into BodyKey (as ReadBody would), runs ContextAssembler, returns the context
// (for body/template_id inspection) + recorder (for abort status/body).
func runAssembler(t *testing.T, reg *service.TemplateRegistry, path string, bodyObj gin.H) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	bodyBytes, _ := json.Marshal(bodyObj)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bodyBytes))
	c.Set(BodyKey, bodyBytes)
	ContextAssembler(reg, nil)(c)
	return c, w
}

func plain(name, system, vars string) *model.PromptTemplate {
	return &model.PromptTemplate{
		Name: name, SystemPrompt: system, TargetFormat: "auto", Status: 1,
		VariablesSchema: datatypes.JSON([]byte(vars)),
	}
}

func TestContextAssembler_NoXContextPassthrough(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t)
	orig := gin.H{"model": "gpt-4o", "messages": []gin.H{{"role": "user", "content": "hi"}}}
	origBytes, _ := json.Marshal(orig)
	c, w := runAssembler(t, reg, "/v1/chat/completions", orig)
	assert.Equal(t, http.StatusOK, w.Code)
	// body unchanged (no x_context → no-op)
	assert.Equal(t, origBytes, GetBodyBytes(c), "passthrough must not alter body")
	_, ok := c.Get("template_id")
	assert.False(t, ok, "no template_id when no x_context")
}

func TestContextAssembler_OpenAIInjectsSystemAndRemovesXContext(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t, plain("greet", "Be brief. Lang={{lang}}.", `[{"name":"lang","trusted":true}]`))
	c, w := runAssembler(t, reg, "/v1/chat/completions", gin.H{
		"x_context": gin.H{"template": "greet", "variables": gin.H{"lang": "中文"}},
		"model":     "gpt-4o",
		"messages":  []gin.H{{"role": "user", "content": "hi"}},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// B1: both BodyKey and c.Request.Body updated consistently.
	bodyBytes := GetBodyBytes(c)
	reqBody, _ := io.ReadAll(c.Request.Body)
	assert.Equal(t, bodyBytes, reqBody, "BodyKey and c.Request.Body must match (B1 double-update)")

	var got map[string]any
	require.NoError(t, json.Unmarshal(bodyBytes, &got))
	assert.Nil(t, got["x_context"], "x_context must be removed")
	msgs := got["messages"].([]any)
	first := msgs[0].(map[string]any)
	assert.Equal(t, "system", first["role"])
	assert.Equal(t, "Be brief. Lang=中文.", first["content"])
	assert.Equal(t, "hi", msgs[1].(map[string]any)["content"], "original user message preserved")

	// template_id threaded into context for usage logging.
	tid, ok := c.Get("template_id")
	require.True(t, ok)
	assert.NotZero(t, tid)
}

func TestContextAssembler_MultiTurnHistoryPreserved(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t, plain("cs", "You are support.", `[]`))
	c, w := runAssembler(t, reg, "/v1/chat/completions", gin.H{
		"x_context": gin.H{"template": "cs"},
		"messages": []gin.H{
			{"role": "user", "content": "你好"},
			{"role": "assistant", "content": "你好！有什么可以帮你？"},
			{"role": "user", "content": "我想退货"},
		},
	})
	require.Equal(t, http.StatusOK, w.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(GetBodyBytes(c), &got))
	msgs := got["messages"].([]any)
	// 1 injected system + 3 original = 4; history order preserved.
	require.Len(t, msgs, 4)
	assert.Equal(t, "system", msgs[0].(map[string]any)["role"])
	assert.Equal(t, "你好", msgs[1].(map[string]any)["content"])
	assert.Equal(t, "我想退货", msgs[3].(map[string]any)["content"])
}

func TestContextAssembler_AnthropicInjectsSystemField(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t, plain("cs", "You are Claude support.", `[]`))
	c, w := runAssembler(t, reg, "/v1/messages", gin.H{
		"x_context": gin.H{"template": "cs"},
		"model":     "claude-sonnet-4-5",
		"messages":  []gin.H{{"role": "user", "content": "hi"}},
	})
	require.Equal(t, http.StatusOK, w.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(GetBodyBytes(c), &got))
	assert.Nil(t, got["x_context"])
	assert.Equal(t, "You are Claude support.", got["system"])
}

func TestContextAssembler_SystemConflictRejected(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t, plain("cs", "sys", `[]`))
	_, w := runAssembler(t, reg, "/v1/chat/completions", gin.H{
		"x_context": gin.H{"template": "cs"},
		"messages": []gin.H{
			{"role": "system", "content": "my own system"},
			{"role": "user", "content": "hi"},
		},
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "system_conflict")
}

func TestContextAssembler_TemplateNotFound(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t)
	_, w := runAssembler(t, reg, "/v1/chat/completions", gin.H{
		"x_context": gin.H{"template": "ghost"},
		"messages":  []gin.H{{"role": "user", "content": "hi"}},
	})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestContextAssembler_RenderErrorRejected(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t, plain("bad", "Q: {{user_input}}", `[]`))
	_, w := runAssembler(t, reg, "/v1/chat/completions", gin.H{
		"x_context": gin.H{"template": "bad", "variables": gin.H{"user_input": "x"}},
		"messages":  []gin.H{{"role": "user", "content": "hi"}},
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "untrusted_var_in_system")
}

// TestContextAssembler_AssemblerHookDeny: a registered hook (e.g. enterprise
// per-Key template permission) returning an error must abort with 403 before
// any assembly. Community ships hook=nil; this is the commercial seam.
func TestContextAssembler_AssemblerHookDeny(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t, plain("secret", "sys", `[]`))
	hook := AssemblerHook(func(c *gin.Context, tpl *model.PromptTemplate, vars map[string]any) error {
		return errors.New("template_not_allowed")
	})
	_, w := runAssemblerWithHook(t, reg, hook, "/v1/chat/completions", gin.H{
		"x_context": gin.H{"template": "secret"},
		"messages":  []gin.H{{"role": "user", "content": "hi"}},
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "template_not_allowed")
}

// runAssemblerWithHook is like runAssembler but injects a hook into ContextAssembler.
func runAssemblerWithHook(t *testing.T, reg *service.TemplateRegistry, hook AssemblerHook, path string, bodyObj gin.H) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	bodyBytes, _ := json.Marshal(bodyObj)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bodyBytes))
	c.Set(BodyKey, bodyBytes)
	ContextAssembler(reg, hook)(c)
	return c, w
}
