package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crosslink/internal/apidoc"
)

func setupVersionRouter(apiVersion string, deprecations []apidoc.Deprecation) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(APIVersion(apiVersion, func() []apidoc.Deprecation { return deprecations }))
	r.GET("/v1/models", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/portal/api/usage", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func TestAPIVersion_HeadersOnEveryResponse(t *testing.T) {
	r := setupVersionRouter("v1", nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "v1", w.Header().Get("X-API-Version"))
	assert.Empty(t, w.Header().Get("Deprecation"))
	assert.Empty(t, w.Header().Get("Sunset"))
}

func TestAPIVersion_DeprecatedRouteGetsHeaders(t *testing.T) {
	sunset := time.Date(2027, 10, 6, 0, 0, 0, 0, time.UTC)
	r := setupVersionRouter("v1", []apidoc.Deprecation{{
		Route:        "GET /portal/api/usage",
		DeprecatedAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC),
		SunsetAt:     sunset,
		Successor:    "https://docs.example.com/api/usage",
		Reason:       "moving to /v1/usage",
	}})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/portal/api/usage", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "v1", w.Header().Get("X-API-Version"))
	assert.Equal(t, "true", w.Header().Get("Deprecation"))
	assert.Equal(t, sunset.Format(http.TimeFormat), w.Header().Get("Sunset"))
	assert.Contains(t, w.Header().Get("Link"), `rel="successor-version"`)
}

func TestAPIVersion_DeprecationDoesNotLeakToOtherRoutes(t *testing.T) {
	r := setupVersionRouter("v1", []apidoc.Deprecation{{
		Route:        "GET /portal/api/usage",
		DeprecatedAt: time.Now(),
		SunsetAt:     time.Now().Add(365 * 24 * time.Hour),
		Reason:       "moving to /v1/usage",
	}})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Deprecation"))
}
