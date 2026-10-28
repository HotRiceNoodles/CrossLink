package guardapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// M3 console embed：占位页托管、SPA fallback、不干扰其他路由组。
func newConsoleRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/api/v1/detect", func(c *gin.Context) { c.String(http.StatusOK, "api") })
	registerConsoleRoutes(r)
	return r
}

func doGET(r *gin.Engine, path string) (int, string) {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code, w.Body.String()
}

func TestConsoleEmbed(t *testing.T) {
	r := newConsoleRouter()

	// /console → 301 到 /console/；/console/ 命中 index（仓内默认为占位页，
	// 构建期被真实 dist 覆盖，断言内容无关）
	if code, _ := doGET(r, "/console"); code != http.StatusMovedPermanently {
		t.Fatalf("redirect: %d", code)
	}
	code, body := doGET(r, "/console/")
	if code != http.StatusOK || !strings.Contains(body, "<!DOCTYPE html") {
		t.Fatalf("index: %d %.80s", code, body)
	}
	// 前端路由 fallback：未命中资源路径回 index.html
	if code, body := doGET(r, "/console/tenants/42/alerts"); code != http.StatusOK ||
		!strings.Contains(body, "<!DOCTYPE html") {
		t.Fatalf("spa fallback: %d %.80s", code, body)
	}
	// 显式资源命中
	if code, _ := doGET(r, "/console/index.html"); code != http.StatusOK {
		t.Fatalf("index.html: %d", code)
	}
	// 其他路由组不受影响
	if code, body := doGET(r, "/healthz"); code != http.StatusOK || body != "ok" {
		t.Fatalf("healthz: %d %s", code, body)
	}
	if code, body := doGET(r, "/api/v1/detect"); code != http.StatusOK || body != "api" {
		t.Fatalf("api: %d %s", code, body)
	}
}
