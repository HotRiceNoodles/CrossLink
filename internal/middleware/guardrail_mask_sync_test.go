package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Regression: applyRequestMask used to rewrite only c.Request.Body. Every
// downstream consumer (cache key, handlers, usage logging) reads the buffered
// copy via GetBodyBytes in preference to the request body, so the mask was
// silently discarded and the unmasked original reached the upstream.
func TestApplyRequestMaskSyncsBodyBuffer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ReadBody(10 << 20))
	r.Use(func(c *gin.Context) {
		ex := extractContent(GetBodyBytes(c), "/v1/chat/completions")
		if ex == nil {
			t.Fatalf("extractContent returned nil for fixture body")
		}
		applyRequestMask(c, ex, &guardrail.CheckResponse{
			Action:      "mask",
			MaskedSpans: []guardrail.Span{{Start: 0, End: 6, Replacement: "[MASKED]"}},
		})
		c.Next()
	})
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		got := GetBodyBytes(c)
		if bytes.Contains(got, []byte("SECRET")) {
			c.String(http.StatusInternalServerError, "leak: buffer still holds the unmasked original")
			return
		}
		if !bytes.Contains(got, []byte("[MASKED]")) {
			c.String(http.StatusInternalServerError, "buffer does not hold the masked rewrite")
			return
		}
		body, _ := io.ReadAll(c.Request.Body)
		if !bytes.Equal(body, got) {
			c.String(http.StatusInternalServerError, "request body and buffer diverge after mask")
			return
		}
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"SECRET"}]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("mask did not sync body buffer: %d %s", w.Code, w.Body.String())
	}
}

// Regression: the guardrail buffered multipart image-edit uploads through a
// truncated (10MB) copy and replaced the request body with it, corrupting the
// multipart framing of any larger upload. It must pass multipart file-upload
// bodies through untouched (non-strict mode).
func TestGuardrailsRequestMultipartImagePassesThroughUnmodified(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	for _, stmt := range []string{
		"CREATE TABLE system_settings (key TEXT, value TEXT)",
		"INSERT INTO system_settings VALUES ('guardrails_enabled','true')",
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	svc := guardrail.NewGuardrailService(db, nil)
	svc.RefreshSettings(t.Context())
	if !svc.IsEnabled() {
		t.Fatal("fixture guardrail service not enabled")
	}

	// 11MB multipart body — larger than maxGuardrailBodySize, so any
	// buffering through LimitReader would visibly truncate it.
	var payload bytes.Buffer
	payload.WriteString("--BOUNDARY\r\nContent-Disposition: form-data; name=\"image\"; filename=\"a.png\"\r\nContent-Type: image/png\r\n\r\n")
	chunk := bytes.Repeat([]byte{0xAB}, 11<<20)
	payload.Write(chunk)
	payload.WriteString("\r\n--BOUNDARY--\r\n")
	original := payload.Bytes()

	r := gin.New()
	r.Use(GuardrailsRequest(svc))
	r.POST("/v1/images/edits", func(c *gin.Context) {
		got, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.String(http.StatusInternalServerError, "read: %v", err)
			return
		}
		if len(got) != len(original) {
			c.String(http.StatusInternalServerError, "body truncated: got %d want %d", len(got), len(original))
			return
		}
		if !bytes.Equal(got, original) {
			c.String(http.StatusInternalServerError, "body bytes modified by guardrail")
			return
		}
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(original))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=BOUNDARY")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("multipart image upload corrupted: %d %s", w.Code, w.Body.String())
	}
}

// The assembler fast-path must not change passthrough semantics for bodies
// that merely contain the x_context literal inside a string value (no actual
// field), and must still assemble real x_context requests.
func TestContextAssemblerFastPathPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ReadBody(10 << 20))
	r.Use(ContextAssembler(nil, nil)) // nil registry: template lookup only happens for real x_context
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		body := GetBodyBytes(c)
		if !bytes.Contains(body, []byte("literal x_context mention")) {
			c.String(http.StatusInternalServerError, "body mutated on passthrough")
			return
		}
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"literal x_context mention but no field"}]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("fast-path changed passthrough semantics: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "template_not_found") {
		t.Fatal("literal substring must not trigger template resolution")
	}
}
