package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
)

// mockKeyValidator implements KeyValidator for testing
type mockKeyValidator struct {
	key *model.APIKey
	err error
}

func (m *mockKeyValidator) Validate(ctx context.Context, rawKey string) (*model.APIKey, error) {
	return m.key, m.err
}

func TestMCPAuth_MissingAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(MCPAuth("test-config-key", nil))
	r.POST("/:server", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestMCPAuth_ConfigKeyMatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(MCPAuth("test-config-key", nil))
	r.POST("/:server", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("x-api-key", "test-config-key")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestMCPAuth_DBKeyValid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	validator := &mockKeyValidator{
		key: &model.APIKey{ID: 42},
	}
	r := gin.New()
	r.Use(MCPAuth("config-key", validator))
	r.POST("/:server", func(c *gin.Context) {
		keyID, _ := c.Get("api_key_id")
		authVia, _ := c.Get("auth_via")
		c.JSON(200, gin.H{"key_id": keyID, "auth_via": authVia})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("x-api-key", "db-key-123")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestMCPAuth_DBKeyExpired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	validator := &mockKeyValidator{err: errKeyExpired}
	r := gin.New()
	r.Use(MCPAuth("config-key", validator))
	r.POST("/:server", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("x-api-key", "expired-key")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestMCPAuth_DBKeyInvalid_ConfigFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	validator := &mockKeyValidator{err: errors.New("not found")}
	r := gin.New()
	r.Use(MCPAuth("config-key", validator))
	r.POST("/:server", func(c *gin.Context) {
		authVia, _ := c.Get("auth_via")
		c.JSON(200, gin.H{"auth_via": authVia})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("x-api-key", "config-key")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}
