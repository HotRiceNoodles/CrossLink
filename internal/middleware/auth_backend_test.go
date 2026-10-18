package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	sqlite "github.com/glebarez/sqlite"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// --- Auth-backend-unavailable (review fix): infrastructure failures are NOT
// credential verdicts. The middleware must answer 503 + Retry-After and must
// NOT record an auth failure — a valid key recovers automatically once the
// backend returns and must never be IP-locked by a DB outage.

// newKeyServiceWithClosedDB builds a KeyService whose hash repo sits on a
// CLOSED database: every lookup fails with an infrastructure error, forcing
// Validate onto the ErrAuthUnavailable path (auth cache deliberately empty).
func newKeyServiceWithClosedDB(t *testing.T) (*service.KeyService, *redis.Client) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("raw db: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatalf("crypto provider: %v", err)
	}
	return service.NewKeyService(nil, repository.NewAPIKeyHashRepo(gdb), nil, cp, rdb), rdb
}

func TestAuth_BackendUnavailableIs503Not401AndNeverLocks(t *testing.T) {
	keySvc, rdb := newKeyServiceWithClosedDB(t)
	r := setupRouter()
	r.Use(Auth("", keySvc, rdb, service.NoopPolicy{}))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	for i := 0; i < 30; i++ { // far beyond the lock threshold
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("x-api-key", "valid-looking-key")
		req.RemoteAddr = "10.1.2.3:5555"
		r.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("request %d: expected 503 (auth backend down), got %d body=%s", i, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got == "" {
			t.Fatalf("request %d: 503 must carry Retry-After", i)
		}
	}
	// The IP auth-failure counter must remain ABSENT: infra errors never count.
	if err := rdb.Get(context.Background(), "auth_fail:10.1.2.3").Err(); err == nil {
		t.Fatal("auth_fail counter must not exist after backend failures")
	}
	// Service-level classification: the error must be ErrAuthUnavailable, so
	// callers can distinguish infrastructure from credential verdicts.
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sdb, _ := gdb.DB()
	_ = sdb.Close()
	cp, _ := crypto.NewProvider("standard")
	ks := service.NewKeyService(nil, repository.NewAPIKeyHashRepo(gdb), nil, cp, nil)
	if _, err := ks.Validate(context.Background(), "some-key"); err == nil || !isAuthUnavailable(err) {
		t.Fatalf("closed-DB Validate must return ErrAuthUnavailable, got %v", err)
	}
}

func isAuthUnavailable(err error) bool {
	const marker = "auth backend unavailable"
	return err != nil && len(err.Error()) >= len(marker) && err.Error()[:len(marker)] == marker
}

// A genuinely invalid key (backend HEALTHY) still 401s and still counts — the
// distinction must not weaken real credential rejection.
func TestAuth_InvalidKeyStillCountsWhenBackendHealthy(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.APIKey{}, &model.APIKeyHash{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cp, _ := crypto.NewProvider("standard")
	keySvc := service.NewKeyService(nil, repository.NewAPIKeyHashRepo(gdb), nil, cp, rdb)

	r := setupRouter()
	r.Use(Auth("", keySvc, rdb, service.NoopPolicy{}))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("x-api-key", "wrong-key")
	req.RemoteAddr = "10.1.2.4:5555"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("healthy backend + unknown key must stay 401, got %d", w.Code)
	}
	n, _ := rdb.Get(context.Background(), "auth_fail:10.1.2.4").Int()
	if n != 1 {
		t.Fatalf("genuine invalid key must record exactly 1 failure, got %d", n)
	}
}
