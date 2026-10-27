package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestReadBodyEmptyRequestDoesNotRequireBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		budget *InflightBodyBudget
	}{
		{"nil", nil},
		{"disabled", NewInflightBodyBudget(0)},
		{"saturated", NewInflightBodyBudget(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "saturated" {
				if !tc.budget.Acquire(1) {
					t.Fatal("预占预算失败")
				}
				defer tc.budget.Release(1)
			}
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				r := gin.New()
				r.Use(ReadBodyWithBudget(1024, tc.budget, 0))
				r.Handle(method, "/v1/models", func(c *gin.Context) {
					if len(GetBodyBytes(c)) != 0 {
						t.Error("空请求被添加了请求体")
					}
					c.Status(http.StatusNoContent)
				})
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(method, "/v1/models", nil))
				if w.Code != http.StatusNoContent {
					t.Fatalf("%s 空请求返回 %d: %s", method, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestReadBodyUnknownLengthWithDisabledBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, limit := range []int64{0, -1} {
		for _, budget := range []*InflightBodyBudget{nil, NewInflightBodyBudget(limit)} {
			for _, body := range []string{"", "hello", strings.Repeat("x", 17)} {
				r := gin.New()
				r.Use(ReadBodyWithBudget(16, budget, 0))
				r.POST("/v1/chat/completions", func(c *gin.Context) { c.String(http.StatusOK, string(GetBodyBytes(c))) })
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
				req.ContentLength = -1
				req.TransferEncoding = []string{"chunked"}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if len(body) > 16 {
					if w.Code != http.StatusRequestEntityTooLarge {
						t.Fatalf("超限请求返回 %d，预期 413", w.Code)
					}
				} else if w.Code != http.StatusOK || w.Body.String() != body {
					t.Fatalf("未知长度请求返回 %d: %q", w.Code, w.Body.String())
				}
			}
		}
	}
}
