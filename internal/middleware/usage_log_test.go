package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestLogRouteTypeFromPath(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{"/v1/messages", "anthropic"},
		{"/v1/chat/completions", "openai"},
		{"/v1/unknown", "unknown"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			got := logRouteTypeFromPath(tt.pattern)
			if got != tt.want {
				t.Errorf("logRouteTypeFromPath(%q) = %q, want %q", tt.pattern, got, tt.want)
			}
		})
	}
}

func TestMapStatusToErrorType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name   string
		status int
		ctxSet map[string]bool
		want   string
	}{
		{"429 rate_limit", 429, nil, "rate_limit"},
		{"429 budget_exceeded", 429, map[string]bool{"budget_exceeded": true}, "budget_exceeded"},
		{"429 call_limit_exceeded", 429, map[string]bool{"call_limit_exceeded": true}, "call_limit_exceeded"},
		{"400", 400, nil, "bad_request"},
		{"401", 401, nil, "auth_failure"},
		{"403", 403, nil, "forbidden"},
		{"404", 404, nil, "not_found"},
		{"503", 503, nil, "service_unavailable"},
		{"500", 500, nil, "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tt.ctxSet {
				c.Set(k, v)
			}
			got := mapStatusToErrorType(c, tt.status)
			if got != tt.want {
				t.Errorf("mapStatusToErrorType(_, %d) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}
