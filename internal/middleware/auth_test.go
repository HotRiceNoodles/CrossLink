package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"gorm.io/datatypes"
)

func setupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	return r
}

func TestAuth_MissingAPIKey(t *testing.T) {
	r := setupRouter()
	r.Use(Auth("secret", nil, nil))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuth_ConfigKeyMatch(t *testing.T) {
	r := setupRouter()
	r.Use(Auth("my-secret-key", nil, nil))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("x-api-key", "my-secret-key")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuth_BearerToken(t *testing.T) {
	r := setupRouter()
	r.Use(Auth("my-key", nil, nil))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer my-key")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuth_InvalidKey(t *testing.T) {
	r := setupRouter()
	r.Use(Auth("correct-key", nil, nil))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("x-api-key", "wrong-key")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRequireRoute_Allowed(t *testing.T) {
	r := setupRouter()
	r.Use(func(c *gin.Context) {
		c.Set("api_key", &model.APIKey{
			AllowedRoutes: datatypes.JSON(`["anthropic","openai"]`),
		})
		c.Next()
	})
	r.Use(RequireRoute("openai"))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRequireRoute_Blocked(t *testing.T) {
	r := setupRouter()
	r.Use(func(c *gin.Context) {
		c.Set("api_key", &model.APIKey{
			AllowedRoutes: datatypes.JSON(`["anthropic"]`),
		})
		c.Next()
	})
	r.Use(RequireRoute("openai"))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRequireRoute_NoKey(t *testing.T) {
	r := setupRouter()
	r.Use(RequireRoute("openai"))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (no key = no restriction), got %d", w.Code)
	}
}

func TestRequireModel_Allowed(t *testing.T) {
	r := setupRouter()
	r.Use(func(c *gin.Context) {
		c.Set("api_key", &model.APIKey{
			AllowedModels: datatypes.JSON(`["gpt-4","claude-3"]`),
		})
		c.Next()
	})
	r.Use(RequireModel())
	r.POST("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test", strings.NewReader(`{"model":"claude-3"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRequireModel_Blocked(t *testing.T) {
	r := setupRouter()
	r.Use(func(c *gin.Context) {
		c.Set("api_key", &model.APIKey{
			AllowedModels: datatypes.JSON(`["gpt-4"]`),
		})
		c.Next()
	})
	r.Use(RequireModel())
	r.POST("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test", strings.NewReader(`{"model":"claude-3"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRequireModel_EmptyAllowed(t *testing.T) {
	r := setupRouter()
	r.Use(func(c *gin.Context) {
		c.Set("api_key", &model.APIKey{})
		c.Next()
	})
	r.Use(RequireModel())
	r.POST("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test", strings.NewReader(`{"model":"claude-3"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (empty allowed_models = all allowed), got %d", w.Code)
	}
}
