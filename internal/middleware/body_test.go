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

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := strings.Repeat("x", 100)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	ReadBody(10)(c)

	got := GetBodyBytes(c)
	if len(got) != 10 {
		t.Errorf("GetBodyBytes() len = %d, want 10", len(got))
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
