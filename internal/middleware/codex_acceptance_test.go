package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestCodexAcceptanceStructuredMaskPreservesFields(t *testing.T) {
	tests := []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"messages":[{"role":"system","content":"SECRET"},{"role":"user","content":[{"type":"text","text":"SECRET"},{"type":"image_url","image_url":{"url":"https://example.invalid/img.png"}}]},{"role":"assistant","tool_calls":[{"id":"call1","type":"function","function":{"name":"send","arguments":"{\"address\":\"SECRET\"}"}}]}],"tools":[{"type":"function","function":{"name":"send","description":"SECRET"}}],"custom":{"keep":42}}`},
		{"anthropic", "/v1/messages", `{"system":[{"type":"text","text":"SECRET"}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"lookup","input":{"query":"SECRET"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":[{"type":"text","text":"SECRET"},{"type":"text","text":"SECRET"}]}]}],"custom":{"keep":42}}`},
		{"responses", "/v1/responses", `{"instructions":"SECRET","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET"}]},{"type":"function_call_output","call_id":"call1","output":"SECRET"},{"type":"function_call","name":"lookup","arguments":"{\"query\":\"SECRET\"}"}],"custom":{"keep":42}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ex := extractContent([]byte(tc.body), tc.path)
			if ex == nil {
				t.Fatal("expected structured checkable content")
			}
			var spans []guardrail.Span
			for offset := 0; offset < len(ex.text); {
				i := strings.Index(ex.text[offset:], "SECRET")
				if i < 0 {
					break
				}
				i += offset
				spans = append(spans, guardrail.Span{Start: i, End: i + 6, Replacement: "[MASKED]"})
				offset = i + 6
			}
			body, ok := ex.applyMask(spans)
			if !ok || !json.Valid(body) {
				t.Fatalf("mask failed or invalid JSON: %s", body)
			}
			if strings.Contains(string(body), "SECRET") {
				t.Fatalf("a checkable field was not extracted or not serialized back: %s", body)
			}
			var before, after map[string]any
			_ = json.Unmarshal([]byte(tc.body), &before)
			_ = json.Unmarshal(body, &after)
			b, _ := json.Marshal(before["custom"])
			a, _ := json.Marshal(after["custom"])
			if string(a) != string(b) {
				t.Fatal("unknown fields changed")
			}
			if tc.name == "chat" && !strings.Contains(string(body), "https://example.invalid/img.png") {
				t.Fatal("media block dropped")
			}
		})
	}
}

type codexAcceptanceCache struct{ body []byte }

func (f codexAcceptanceCache) IsEnabled() bool  { return true }
func (f codexAcceptanceCache) MaxBodySize() int { return 1 << 20 }
func (f codexAcceptanceCache) GetModelCacheConfig(context.Context, string) (time.Duration, bool) {
	return 0, false
}
func (f codexAcceptanceCache) GetTTLForEndpoint(string) time.Duration { return time.Minute }
func (f codexAcceptanceCache) Get(context.Context, string) (*service.CacheEntry, bool) {
	// Priced: this test targets the guardrail re-check, not the unpriced
	// legacy-entry bypass; a priced entry reaches the guardrail branch.
	return &service.CacheEntry{Body: f.body, Priced: true}, true
}
func (f codexAcceptanceCache) Set(context.Context, string, string, string, []byte, time.Duration, float64, float64, string) error {
	return nil
}

func TestCodexAcceptanceCacheHitIsRechecked(t *testing.T) {
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
		"CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, name TEXT, type TEXT, direction TEXT, enabled BOOLEAN, config TEXT, severity TEXT, action TEXT)",
		`INSERT INTO guardrail_rules VALUES (1,'synthetic','keyword_filter','response',true,'{"blocklist":["SECRET"]}','high','block')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	svc := guardrail.NewGuardrailService(db, nil)
	svc.RefreshSettings(t.Context())
	fake := codexAcceptanceCache{body: []byte(`{"choices":[{"message":{"content":"SECRET"}}]}`)}
	r := gin.New()
	r.Use(Cache(fake, testCP(), svc, nil))
	calls := 0
	r.POST("/v1/chat/completions", func(c *gin.Context) { calls++; c.String(200, "safe regenerated response") })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"synthetic","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if calls != 1 || strings.Contains(w.Body.String(), "SECRET") || w.Header().Get("X-Cache") != "BYPASS-GUARDRAIL" {
		t.Fatalf("stale cache policy bypass: calls=%d response=%s", calls, w.Body.String())
	}
}

func TestCodexAcceptanceStrictCoverageRejectsMixedMedia(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err := db.Exec("CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, enabled BOOLEAN)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE system_settings (key TEXT, value TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO system_settings (key, value) VALUES ('guardrails_enabled','true'), ('guardrails_strict_coverage','true')").Error; err != nil {
		t.Fatal(err)
	}
	svc := guardrail.NewGuardrailService(db, nil)
	svc.RefreshSettings(t.Context())
	if !svc.StrictCoverage() {
		t.Fatal("fixture strict coverage not enabled")
	}
	r := gin.New()
	r.Use(GuardrailsRequest(svc))
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"benign text"},{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code >= 200 && w.Code < 300 {
		t.Fatalf("strict coverage silently forwarded unsupported image next to safe text: %d", w.Code)
	}
}

func TestCodexAcceptanceToolJSONEscapesAreDecodedForDetection(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"SE\\u0043RET\"}"}}]}]}`},
		{"anthropic", "/v1/messages", `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"lookup","input":{"query":"SE\u0043RET"}}]}]}`},
		{"responses", "/v1/responses", `{"input":[{"type":"function_call","name":"lookup","arguments":"{\"query\":\"SE\\u0043RET\"}"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := extractContent([]byte(tc.body), tc.path)
			if ex == nil || !strings.Contains(ex.text, "SECRET") {
				t.Fatalf("decoded tool argument text missing from detection input: %#v", ex)
			}
		})
	}
}

func TestCodexAcceptanceUnappliedMaskCannotForwardOriginal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ex := extractContent([]byte(`{"messages":[{"role":"user","content":"SECRET"}]}`), "/v1/chat/completions")
		applyRequestMask(c, ex, &guardrail.CheckResponse{Action: "mask", MaskedContent: "[MASKED]"})
		c.Next()
	})
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if w.Code >= 200 && w.Code < 300 {
		t.Fatalf("mask action could not be applied yet original request was allowed: %d", w.Code)
	}
}
