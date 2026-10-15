package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Regression (supervisor review): the bytes.Contains fast path skipped
// template resolution for ESCAPED JSON key names — `{"\u0078_context":…}`
// unmarshals to the same x_context field but never matches the literal
// `"x_context"` probe, silently bypassing expansion AND the Enterprise
// permission hook. Bodies containing any backslash now fall back to the full
// parse. Fixtures are built as RAW JSON BYTES (no language-level unescaping
// of the escape sequence) so the middleware sees the literal backslash-u form.
func TestContextAssembler_EscapedKeyStillResolves(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t, plain("existing-template", "You are {name}.", `[]`))

	hookCalled := false
	hook := func(c *gin.Context, tpl *model.PromptTemplate, variables map[string]any) error {
		hookCalled = true
		return nil
	}

	// Raw JSON bytes: top-level key is the ESCAPED form \u0078_context.
	// Built via string concatenation so no compiler unescaping can occur.
	raw := []byte(`{"` + `\u0078` + `_context":{"template":"existing-template"},"messages":[{"role":"user","content":"hi"}]}`)
	require.True(t, bytes.Contains(raw, []byte(`\u0078_context`)), "fixture must carry the raw escape sequence")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	c.Set(BodyKey, raw)
	ContextAssembler(reg, hook)(c)

	if w.Code >= 400 {
		t.Fatalf("escaped-key request aborted: %d %s", w.Code, w.Body.String())
	}
	if !hookCalled {
		t.Fatal("escaped x_context key bypassed template resolution and the permission hook")
	}
	// Assembly must have consumed the (escaped) x_context field.
	if bytes.Contains(GetBodyBytes(c), []byte("u0078_context")) {
		t.Fatal("x_context not stripped from the assembled body")
	}
}

// Content mentioning x_context WITHOUT a top-level key must stay passthrough
// — both with and without backslashes elsewhere in the body.
func TestContextAssembler_LiteralInContentOnlyStillPassthrough(t *testing.T) {
	reg, _ := setupAssemblerRegistry(t, plain("existing-template", "sys", `[]`))
	hookCalled := false
	hook := func(c *gin.Context, tpl *model.PromptTemplate, variables map[string]any) error {
		hookCalled = true
		return nil
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		// No backslash: literal probe hits, full parse finds no field -> passthrough.
		{"plain-content", `{"model":"m","messages":[{"role":"user","content":"please use the x_context field"}]}`},
		// Backslash (escaped newline in content) forces the safe parse path;
		// the string VALUE mentions x_context but no such key exists.
		{"escaped-content", `{"model":"m","messages":[{"role":"user","content":"use x_context` + `\n` + `second line"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookCalled = false
			raw := []byte(tc.body)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
			c.Set(BodyKey, raw)
			ContextAssembler(reg, hook)(c)
			if w.Code >= 400 {
				t.Fatalf("passthrough aborted: %d %s", w.Code, w.Body.String())
			}
			if hookCalled {
				t.Fatal("content-only mention triggered template resolution")
			}
			if !strings.Contains(string(GetBodyBytes(c)), "x_context") {
				t.Fatal("passthrough must not mutate the body")
			}
		})
	}
}
