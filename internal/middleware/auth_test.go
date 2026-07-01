package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
)

func setupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	return r
}

func TestAuth_MissingAPIKey(t *testing.T) {
	r := setupRouter()
	r.Use(Auth("secret", nil, nil, service.NoopPolicy{}))
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
	r.Use(Auth("my-secret-key", nil, nil, service.NoopPolicy{}))
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
	r.Use(Auth("my-key", nil, nil, service.NoopPolicy{}))
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
	r.Use(Auth("correct-key", nil, nil, service.NoopPolicy{}))
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

// --- IP binding (IPPolicy) tests ---

// denyAllIPPolicy denies any non-empty client IP. Used to exercise the
// DB-key-valid + IP-mismatch branch of Auth.
type denyAllIPPolicy struct{}

func (denyAllIPPolicy) Check(k *model.APIKey, ip, lang string) error {
	if ip == "" {
		return nil
	}
	return errors.New("ip not allowed")
}

// newKeyServiceWithCachedKey builds a *service.KeyService backed by miniredis,
// with the auth cache pre-populated so Validate() returns keyPlain as a valid
// key without touching the database. Returns the KeyService and the redis
// client (the caller usually only needs the KeyService).
func newKeyServiceWithCachedKey(t *testing.T, keyPlain string, key *model.APIKey) (*service.KeyService, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatalf("crypto provider: %v", err)
	}
	hashHex := cp.HashHex([]byte(keyPlain))

	// service.Validate reads authCachePrefix + hashHex; value is JSON of APIKey.
	// authCachePrefix is "auth:key:" (unexported in service), replicated here.
	cacheKey := "auth:key:" + hashHex
	b, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := rdb.Set(context.Background(), cacheKey, b, 0).Err(); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// repo/hashRepo are nil: Validate hits the cache and returns before using them.
	return service.NewKeyService(nil, nil, nil, cp, rdb), rdb
}

func TestAuthIPBindingDenies(t *testing.T) {
	validKey := &model.APIKey{
		ID:        42,
		KeyPrefix: "sk-test",
	}
	keySvc, _ := newKeyServiceWithCachedKey(t, "secret-key-123", validKey)

	r := setupRouter()
	// denyAllIPPolicy: non-empty ClientIP -> deny.
	r.Use(Auth("", keySvc, nil, denyAllIPPolicy{}))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("x-api-key", "secret-key-123")
	req.RemoteAddr = "1.2.3.4:5678" // gives c.ClientIP() a non-empty value
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (IP denied), got %d", w.Code)
	}
	// Response must NOT leak the deny reason.
	body := w.Body.String()
	if strings.Contains(body, "ip not allowed") {
		t.Errorf("response leaked deny reason: %s", body)
	}
	if !strings.Contains(body, "forbidden") {
		t.Errorf("expected generic 'forbidden' message, got: %s", body)
	}
}

// TestAuthIPBindingConfigKeyBypasses locks in the security property that a
// request authenticated via the config auth key (not a DB key) is NOT subject
// to IP binding, even when the policy would deny. Protects against the Check
// being moved into the wrong branch.
func TestAuthIPBindingConfigKeyBypasses(t *testing.T) {
	r := setupRouter()
	// denyAllIPPolicy would deny any DB key, but config-key path must bypass it.
	r.Use(Auth("my-secret-key", nil, nil, denyAllIPPolicy{}))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("x-api-key", "my-secret-key")
	req.RemoteAddr = "1.2.3.4:5678" // non-empty ClientIP that denyAllIPPolicy would reject
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (config key bypasses IP binding), got %d", w.Code)
	}
}

func TestAuthIPBindingNoopAllows(t *testing.T) {
	validKey := &model.APIKey{
		ID:        42,
		KeyPrefix: "sk-test",
	}
	keySvc, _ := newKeyServiceWithCachedKey(t, "secret-key-123", validKey)

	r := setupRouter()
	// NoopPolicy: always allows, even with a non-empty ClientIP.
	r.Use(Auth("", keySvc, nil, service.NoopPolicy{}))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("x-api-key", "secret-key-123")
	req.RemoteAddr = "1.2.3.4:5678"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (NoopPolicy allows), got %d", w.Code)
	}
}
