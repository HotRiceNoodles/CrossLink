package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func incrementalMiddlewareService(t *testing.T, regex bool, override ...guardrail.GuardrailRule) *guardrail.GuardrailService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.SystemSetting{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, org_id INTEGER, name TEXT, type TEXT, direction TEXT, enabled BOOLEAN, config TEXT, severity TEXT, action TEXT, model_filter TEXT, version INTEGER, created_at DATETIME, updated_at DATETIME)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE provider_models (model_name TEXT, extra_config TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	cfg := `{"blocklist":["SECRET"]}`
	if regex {
		cfg = `{"blocklist":["SEC.*RET"],"use_regex":true}`
	}
	rule := guardrail.GuardrailRule{ID: 1, Version: 1, Name: "secrets", Enabled: true, Type: "keyword_filter", Direction: "response", Severity: "high", Action: "block", Config: datatypes.JSON(cfg)}
	if len(override) > 0 {
		rule.Type, rule.Config = override[0].Type, override[0].Config
	}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"guardrails_enabled": "true", "guardrails_stream_mode": "incremental"} {
		if err := db.Create(&model.SystemSetting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	svc := guardrail.NewGuardrailService(db, nil)
	svc.RefreshSettings(t.Context())
	return svc
}

func TestIncrementalMiddlewareLocalEngines(t *testing.T) {
	for _, kind := range []string{"content_length", "credential_detection"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/admin/api/playground/stream", "/admin/api/playground/responses"} {
			t.Run(kind+path, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				cfg := datatypes.JSON(`{"max_output_chars":8}`)
				if kind == "credential_detection" {
					cfg = datatypes.JSON(`{"categories":["api_key"]}`)
				}
				svc := incrementalMiddlewareService(t, false, guardrail.GuardrailRule{Type: kind, Config: cfg})
				r := gin.New()
				r.Use(GuardrailsRequest(svc, nil))
				out := httptest.NewRecorder()
				r.POST(path, func(c *gin.Context) {
					if svc.StreamModeFor(c.Request.Context()) != "incremental" {
						t.Fatal("未安装增量审核")
					}
					c.Header("Content-Type", "text/event-stream")
					write := func(text string) error {
						raw, _ := json.Marshal(gin.H{"choices": []any{gin.H{"index": 0, "delta": gin.H{"content": text}}}})
						_, err := c.Writer.WriteString("data: " + string(raw) + "\n\n")
						return err
					}
					if err := write("安全正文。"); err != nil || !out.Flushed || !strings.Contains(out.Body.String(), "安全正文") {
						t.Fatal("正常正文未立即到达客户端", err)
					}
					ending := "超过字符上限的后文"
					if kind == "credential_detection" {
						if err := write("sk-" + strings.Repeat("Q", 10)); err != nil {
							t.Fatal(err)
						}
						ending = strings.Repeat("Q", 10) + "超过字符上限的后文"
					}
					if err := write(ending); err == nil || !c.GetBool("guardrail_triggered") {
						t.Fatal("没有在当前增量命中时阻断")
					}
				})
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"普通问题"}],"input":"普通问题"}`))
				req.Header.Set("Content-Type", "application/json")
				r.ServeHTTP(out, req)
				if out.Header().Get("X-Guardrail-Stream-Mode") != "incremental" || out.Header().Get("X-Guardrail-Stream-Fallback") != "" || !strings.Contains(out.Body.String(), "guardrail_blocked") || strings.Contains(out.Body.String(), "sk-") || strings.Contains(out.Body.String(), "超过字符") {
					t.Fatal(out.Header(), out.Body.String())
				}
			})
		}
	}
}

func TestIncrementalMiddlewareGuardsAllSSEPaths(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/admin/api/playground/chat", "/admin/api/playground/responses"} {
		t.Run(path, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			svc := incrementalMiddlewareService(t, false)
			r := gin.New()
			r.Use(GuardrailsRequest(svc, nil))
			r.POST(path, func(c *gin.Context) {
				if svc.StreamModeFor(c.Request.Context()) != "incremental" {
					t.Fatal("gate not installed")
				}
				c.Header("Content-Type", "text/event-stream")
				fmt.Fprint(c.Writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"SE\"}}]}\n\n")
				_, err := c.Writer.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"CRET\"}}]}\n\n")
				if err == nil || !c.GetBool("guardrail_triggered") {
					t.Fatal("blocked response not observed by handler")
				}
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hello"}],"input":"hello"}`))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if strings.Contains(w.Body.String(), `"content":"SE"`) || strings.Contains(w.Body.String(), `"content":"CRET"`) {
				t.Fatal(w.Body.String())
			}
			if w.Header().Get("X-Guardrail-Stream-Mode") != "incremental" {
				t.Fatal(w.Header())
			}
		})
	}
}

func TestIncrementalMiddlewareSelectsStrictFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := incrementalMiddlewareService(t, true)
	r := gin.New()
	r.Use(GuardrailsRequest(svc, nil))
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		ch := make(chan domain.SSEChunk, 2)
		ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Index: 0, Delta: domain.OpenAIChunkDelta{Content: "SECRET"}}}}}
		ch <- domain.SSEChunk{Done: true}
		close(ch)
		wrapper := guardrail.NewStreamGuardrailWrapper(ch, svc, "test", 0, 0, 0, c.Request.Context())
		if !wrapper.Strict() || wrapper.Next(c.Request.Context()).Blocked == nil {
			t.Fatal("unsupported rule bypassed strict check")
		}
		c.Status(http.StatusNoContent)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || w.Header().Get("X-Guardrail-Stream-Mode") != "strict" || w.Header().Get("X-Guardrail-Stream-Fallback") == "" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
	}
}

func TestIncrementalMiddlewareDoesNotAlterJSONErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := incrementalMiddlewareService(t, false)
	r := gin.New()
	r.Use(GuardrailsRequest(svc, nil))
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.JSON(400, gin.H{"error": "invalid model"}) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 400 || w.Body.String() != `{"error":"invalid model"}` {
		t.Fatal(w.Code, w.Body.String())
	}
}
