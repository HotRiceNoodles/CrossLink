package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestReadBody_CachesBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))

	ReadBody(1024)(c)

	got := GetBodyBytes(c)
	if string(got) != "hello" {
		t.Errorf("GetBodyBytes() = %q, want %q", string(got), "hello")
	}

	// Body should be re-readable
	body2 := make([]byte, 5)
	n, _ := c.Request.Body.Read(body2)
	if n != 5 || string(body2) != "hello" {
		t.Error("request body should be re-readable")
	}
}

func TestReadBody_ExceedsMaxSize(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Full chain with a downstream sentinel: an oversized body must abort
	// with 413 BEFORE any downstream handler runs — never silently truncated
	// and never forwarded.
	forwarded := false
	r := gin.New()
	r.Use(ReadBody(10))
	r.POST("/", func(c *gin.Context) {
		forwarded = true
		c.Status(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	body := strings.Repeat("x", 100)
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized body, got code=%d", w.Code)
	}
	if forwarded {
		t.Error("oversized body was forwarded to a downstream handler")
	}
	if got := GetBodyBytes(&gin.Context{Request: httptest.NewRequest(http.MethodPost, "/", nil)}); got != nil {
		// sanity: GetBodyBytes with no cached body stays nil
		t.Errorf("GetBodyBytes() = %d bytes, want nil", len(got))
	}
}

func TestGetBodyBytes_NoBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	got := GetBodyBytes(c)
	if got != nil {
		t.Errorf("GetBodyBytes() = %v, want nil", got)
	}
}
