package middleware

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/crosslink/internal/apidoc"
)

// APIDeprecations is the registry the deprecation middleware consults.
// Typed as a function so tests can inject fixtures.
type APIDeprecations func() []apidoc.Deprecation

// APIVersion returns a middleware that stamps every response of an API
// surface with X-API-Version (e.g. "v1") and, for routes listed in the
// deprecation registry, the standard deprecation headers:
//
//	Deprecation: true                  (RFC 9745)
//	Sunset: <HTTP-date>                (RFC 8594)
//	Link: <docs>; rel="deprecation"    (RFC 8288)
//
// Headers are set before c.Next() so they apply to all responses, including
// errors and streaming SSE (headers must be sent before the body starts).
func APIVersion(apiVersion string, deprecations APIDeprecations) gin.HandlerFunc {
	byRoute := make(map[string]apidoc.Deprecation)
	for _, d := range deprecations() {
		byRoute[d.Route] = d
	}

	return func(c *gin.Context) {
		c.Header("X-API-Version", apiVersion)

		if d, ok := byRoute[c.Request.Method+" "+c.FullPath()]; ok {
			c.Header("Deprecation", "true")
			c.Header("Sunset", d.SunsetAt.Format(http.TimeFormat))
			if d.Successor != "" {
				c.Header("Link", fmt.Sprintf("<%s>; rel=\"successor-version\"", d.Successor))
			}
		}

		c.Next()
	}
}
