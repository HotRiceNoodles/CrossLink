package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestID_GeneratesNewID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	RequestID()(c)

	id, exists := c.Get("request_id")
	if !exists {
		t.Fatal("expected request_id to be set")
	}
	if id == "" {
		t.Error("expected non-empty request_id")
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID response header")
	}
}

func TestRequestID_PassesValidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("X-Request-ID", "test-req-123")

	RequestID()(c)

	id, _ := c.Get("request_id")
	if id != "test-req-123" {
		t.Errorf("request_id = %v, want test-req-123", id)
	}
}

func TestRequestID_TruncatesLongID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	longID := strings.Repeat("a", 100)
	c.Request.Header.Set("X-Request-ID", longID)

	RequestID()(c)

	id, _ := c.Get("request_id")
	if len(id.(string)) != 64 {
		t.Errorf("request_id len = %d, want 64", len(id.(string)))
	}
}

func TestRequestID_RejectsInvalidChars(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("X-Request-ID", "abc!def")

	RequestID()(c)

	id, _ := c.Get("request_id")
	// Should generate a new ID (8-char UUID prefix), not use the invalid one
	if id.(string) == "abc!def" {
		t.Error("should not use invalid X-Request-ID")
	}
	if id.(string) == "" {
		t.Error("should generate a fallback ID")
	}
}
