package middleware

import (
	"bytes"
	"context"
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
	r.Use(GuardrailsRequest(svc, nil))
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

func TestCodexAcceptanceUnappliedMaskCannotForwardOriginal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ex := guardrail.ExtractContent([]byte(`{"messages":[{"role":"user","content":"SECRET"}]}`), "/v1/chat/completions")
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
