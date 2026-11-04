package middleware

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIncrementalPlaygroundUsesActualStreamSemantics(t *testing.T) {
	for _, path := range []string{"/admin/api/playground/stream", "/admin/api/playground/chat"} {
		for _, flag := range []string{"", `,"stream":false`, `,"stream":true`} {
			t.Run(path+flag, func(t *testing.T) {
				svc := incrementalMiddlewareService(t, false)
				r := gin.New()
				r.Use(GuardrailsRequest(svc, nil))
				r.POST(path, func(c *gin.Context) {
					want := path == "/admin/api/playground/stream"
					if got := svc.StreamModeFor(c.Request.Context()) == "incremental"; got != want {
						t.Fatalf("实际流式模式错误: got=%v want=%v", got, want)
					}
					c.JSON(200, gin.H{"ok": true})
				})
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hello"}]`+flag+`}`))
				req.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				r.ServeHTTP(out, req)
				if out.Code != 200 {
					t.Fatal(out.Body.String())
				}
			})
		}
	}
}

func TestIncrementalBlockCancelsUpstreamContext(t *testing.T) {
	svc := incrementalMiddlewareService(t, false)
	r := gin.New()
	r.Use(GuardrailsRequest(svc, nil))
	cancelled := false
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		if _, err := c.Writer.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"安全，SECRET 后文\"}}]}\n\n"); err == nil {
			t.Fatal("未阻断")
		}
		cancelled = c.Request.Context().Err() != nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if !cancelled || !strings.Contains(out.Body.String(), "安全，") || strings.Contains(out.Body.String(), "SECRET") || strings.Count(out.Body.String(), "guardrail_blocked") != 1 {
		t.Fatalf("上下文未取消或终止错误: %v %s", cancelled, out.Body.String())
	}
}

func TestOriginalTurnPromptAcrossProtocols(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"system","content":"system"},{"role":"user","content":"原始问题"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"原始问题"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_text","text":"原始问题"}]}]}`,
		`{"input":"原始问题"}`,
	} {
		if got := originalTurnPrompt([]byte(body)); got != "原始问题" {
			t.Fatalf("原始问题丢失: %s => %q", body, got)
		}
	}
}
