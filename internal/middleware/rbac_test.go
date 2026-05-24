package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequireRole_Allowed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, role := range []string{"admin", "member", "viewer"} {
		t.Run(role, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("role_name", role)

			handler := RequireRole(role)
			handler(c)

			if w.Code == http.StatusForbidden {
				t.Errorf("role %s should be allowed", role)
			}
		})
	}
}

func TestRequireRole_Denied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("role_name", "viewer")

	handler := RequireRole("admin")
	handler(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("viewer should be denied, got %d", w.Code)
	}
}

func TestRequireRole_NoRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	handler := RequireRole("admin")
	handler(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("missing role should be denied, got %d", w.Code)
	}
}

func TestRequireRole_MultipleRoles(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := RequireRole("admin", "member")

	// Admin passes
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("role_name", "admin")
	handler(c)
	if w.Code == http.StatusForbidden {
		t.Error("admin should be allowed")
	}

	// Member passes
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set("role_name", "member")
	handler(c)
	if w.Code == http.StatusForbidden {
		t.Error("member should be allowed")
	}

	// Viewer denied
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set("role_name", "viewer")
	handler(c)
	if w.Code != http.StatusForbidden {
		t.Error("viewer should be denied")
	}
}

func TestRequireAction(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &PermissionCache{}
	cache.perms = map[int64]map[string]bool{
		1: {"provider:create": true, "provider:list": true},
		2: {"provider:list": true},
	}

	// Role 1 has provider:create
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("role_id", int64(1))
	RequireAction(cache, "provider:create")(c)
	if w.Code == http.StatusForbidden {
		t.Error("role 1 should have provider:create")
	}

	// Role 2 lacks provider:create
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set("role_id", int64(2))
	RequireAction(cache, "provider:create")(c)
	if w.Code != http.StatusForbidden {
		t.Error("role 2 should be denied provider:create")
	}

	// Unknown role denied
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Set("role_id", int64(99))
	RequireAction(cache, "provider:list")(c)
	if w.Code != http.StatusForbidden {
		t.Error("unknown role should be denied")
	}
}

func TestRequireAction_TierBlocked(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := NewPermissionCache(nil)
	cache.mu.Lock()
	cache.perms = map[int64]map[string]bool{
		1: {"guardrail:list": true, "provider:list": true},
	}
	cache.mu.Unlock()

	tests := []struct {
		name     string
		action   string
		expected int
	}{
		{"community allows provider:list", "provider:list", 200},
		{"community blocks guardrail:list", "guardrail:list", 403},
		{"db missing blocks audit:list", "audit:list", 403},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("role_id", int64(1))
			handler := RequireAction(cache, tt.action)
			handler(c)
			if w.Code != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, w.Code)
			}
		})
	}
}
