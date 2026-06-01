package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCORS_AllowedOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name   string
		origin string
	}{
		{"localhost:5173", "http://localhost:5173"},
		{"localhost:3000", "http://localhost:3000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			c.Request.Header.Set("Origin", tt.origin)

			CORS()(c)

			if w.Header().Get("Access-Control-Allow-Origin") != tt.origin {
				t.Errorf("ACAO header = %q, want %q", w.Header().Get("Access-Control-Allow-Origin"), tt.origin)
			}
		})
	}
}

func TestCORS_BlockedOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("Origin", "http://evil.com")

	CORS()(c)

	if w.Header().Get("Access-Control-Allow-Origin") == "http://evil.com" {
		t.Error("should not set ACAO for blocked origin")
	}
}

func TestCORS_Preflight(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodOptions, "/", nil)
	c.Request.Header.Set("Origin", "http://localhost:5173")

	CORS()(c)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}
