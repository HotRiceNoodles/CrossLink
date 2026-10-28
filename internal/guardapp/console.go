package guardapp

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// 控制台静态托管（设计 DD-8，M3）：go:embed console/dist 由 guard 二进制
// 单进程交付；Community 仓内是占位页，构建期用 crosslink-guard-console
// 仓的产物替换该目录后重新编译（scripts/embed-into-guard.*）即得完整
// 控制台。独立静态部署（nginx 直接 serve dist/）是另一交付模式，不依赖
// 本文件。
//
//go:embed console/dist
var consoleFS embed.FS

// registerConsoleRoutes 挂 /console：SPA 静态托管 + 前端路由 fallback
// （未命中路径回 index.html，前端路由刷新不断链）。/console 以外的路由
// （/api /admin /mirror /metrics /healthz）不受影响；控制台与 Admin API
// 同源部署，无 CORS 面新增。
//
// 文件不经 http.FileServer 输出：FileServer 对 /index.html 发 "./" 规范
// 化重定向，会与 SPA fallback 打架——直接读字节 + MIME 表下发。
func registerConsoleRoutes(r *gin.Engine) {
	dist, err := fs.Sub(consoleFS, "console/dist")
	if err != nil {
		return // 编译期已定，不可达
	}
	r.GET("/console", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/console/")
	})
	r.GET("/console/*path", func(c *gin.Context) {
		p := strings.TrimPrefix(c.Param("path"), "/")
		if p == "" {
			p = "index.html"
		}
		// 路径安全：拒绝 .. 逃逸（embed FS 本身按字面路径查找，双保险）。
		if strings.Contains(p, "..") {
			c.String(http.StatusBadRequest, "invalid path")
			return
		}
		if _, err := fs.Stat(dist, p); err != nil {
			// SPA fallback：非资源路径回 index.html 交前端路由。
			serveConsoleFile(c, dist, "index.html")
			return
		}
		serveConsoleFile(c, dist, p)
	})
}

// consoleMIME 覆盖 vite 产物常用扩展（系统 mime 表缺 web 字体等，显式
// 兜底）；未列出的扩展退 mime.TypeByExtension，再退 octet-stream。
var consoleMIME = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".ico":   "image/x-icon",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
}

func serveConsoleFile(c *gin.Context, dist fs.FS, name string) {
	b, err := fs.ReadFile(dist, name)
	if err != nil {
		c.String(http.StatusNotFound, "console asset not found")
		return
	}
	ct := consoleMIME[strings.ToLower(filepath.Ext(name))]
	if ct == "" {
		ct = mime.TypeByExtension(name)
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	c.Data(http.StatusOK, ct, b)
}
