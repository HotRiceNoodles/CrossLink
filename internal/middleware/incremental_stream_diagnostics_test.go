package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func TestIncrementalHoldHeaderDescribesInstalledCapabilities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		kind, config, want string
	}{
		{"keyword_filter", `{"blocklist":["private-config-value"]}`, "literal_candidate,tool_json_complete"},
		{"keyword_filter", `{"blocklist":["private.*value"],"use_regex":true}`, "tool_json_complete,whole_response_required"},
		{"credential_detection", `{}`, "credential_candidate,tool_json_complete"},
		{"content_length", `{"max_output_chars":100}`, "tool_json_complete"},
	}
	for _, tc := range tests {
		t.Run(tc.kind+tc.config, func(t *testing.T) {
			svc := incrementalMiddlewareService(t, false, guardrail.GuardrailRule{Type: tc.kind, Config: datatypes.JSON(tc.config)})
			router := gin.New()
			router.Use(GuardrailsRequest(svc, nil))
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString("data: [DONE]\n\n")
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
			req.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			router.ServeHTTP(out, req)
			if got := out.Header().Get("X-Guardrail-Stream-Hold"); got != tc.want {
				t.Fatalf("响应头未报告安装链路的能力原因码: got=%q want=%q", got, tc.want)
			}
			if out.Header().Get("X-Guardrail-Stream-Mode") != "incremental" || out.Header().Get("X-Guardrail-Stream-Fallback") != "" {
				t.Fatalf("能力等待被误报成严格回退: %v", out.Header())
			}
		})
	}
}

func TestIncrementalHoldHeaderAbsentWithoutInstalledGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"strict", "incremental"} {
		svc := incrementalMiddlewareService(t, false)
		svc.SetStreamMode(mode)
		router := gin.New()
		router.Use(GuardrailsRequest(svc, nil))
		router.POST("/admin/api/playground/chat", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
		req := httptest.NewRequest(http.MethodPost, "/admin/api/playground/chat", strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		if got := out.Header().Get("X-Guardrail-Stream-Hold"); got != "" {
			t.Fatalf("未安装审核层却报告增量暂留能力: mode=%s got=%q", mode, got)
		}
	}
}
