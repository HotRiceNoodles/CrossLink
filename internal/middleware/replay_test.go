package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Regression tests for the replay-token auth path (external report 2026-10-28):
// the replay branch must authorize on a single-use, unguessable token minted
// in-process — never on "the request source looks loopback". Behind the
// documented same-host nginx proxy every external client arrives with a
// loopback RemoteAddr while server.trusted_proxies is unset, so an
// address-based gate is an authentication bypass.

// newReplayTestEnv builds an Auth-protected router backed by a sqlite
// KeyService. Callers seed keys on the returned DB.
func newReplayTestEnv(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.APIKey{}, &model.APIKeyHash{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatalf("crypto provider: %v", err)
	}
	keySvc := service.NewKeyService(repository.NewAPIKeyRepo(gdb), repository.NewAPIKeyHashRepo(gdb), gdb, cp, nil)

	r := setupRouter()
	// Mirror the production default (app.go): no trusted proxies → gin ignores
	// X-Forwarded-For entirely and ClientIP() is the socket peer address.
	// The stock test router trusts all proxies, which is LOOSER than production.
	_ = r.SetTrustedProxies(nil)
	r.Use(Auth("", keySvc, nil, service.NoopPolicy{}))
	r.GET("/test", func(c *gin.Context) {
		key := GetAPIKeyFromContext(c)
		if key == nil {
			c.JSON(200, gin.H{"authenticated": false})
			return
		}
		c.JSON(200, gin.H{"authenticated": true, "key_id": key.ID, "key_name": key.Name})
	})
	return r, gdb
}

func seedReplayKey(t *testing.T, gdb *gorm.DB, id int64, name string, status int16, expiresAt *time.Time) {
	t.Helper()
	if err := gdb.Create(&model.APIKey{
		ID:        id,
		Name:      name,
		KeyHash:   fmt.Sprintf("hash-%d", id),
		KeyPrefix: "sk-test",
		Status:    status,
		ExpiresAt: expiresAt,
	}).Error; err != nil {
		t.Fatalf("seed key %d: %v", id, err)
	}
}

func replayRequest(r *gin.Engine, token string, remoteAddr string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	if token != "" {
		req.Header.Set("X-Replay-Token", token)
	}
	req.RemoteAddr = remoteAddr
	r.ServeHTTP(w, req)
	return w
}

// The reported bypass vector: a bare key-ID header from a loopback-looking
// source must NOT authenticate (it authenticated as any key before the fix).
func TestReplay_KeyIDHeaderAloneIsRejected(t *testing.T) {
	r, gdb := newReplayTestEnv(t)
	seedReplayKey(t, gdb, 1, "victim-key", 1, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Replay-KeyID", "1") // legacy header, no token, no secret
	req.RemoteAddr = "127.0.0.1:5555"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("X-Replay-KeyID alone must be 401, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestReplay_ValidTokenAuthenticatesAsKey(t *testing.T) {
	r, gdb := newReplayTestEnv(t)
	seedReplayKey(t, gdb, 1, "victim-key", 1, nil)

	w := replayRequest(r, MintReplayToken(1), "127.0.0.1:5555")
	if w.Code != http.StatusOK {
		t.Fatalf("valid minted token must authenticate, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"key_id":1`) {
		t.Fatalf("expected authenticated as key 1, body=%s", w.Body.String())
	}
}

func TestReplay_TokenIsSingleUse(t *testing.T) {
	r, gdb := newReplayTestEnv(t)
	seedReplayKey(t, gdb, 1, "victim-key", 1, nil)

	token := MintReplayToken(1)
	if w := replayRequest(r, token, "127.0.0.1:5555"); w.Code != http.StatusOK {
		t.Fatalf("first use must authenticate, got %d", w.Code)
	}
	if w := replayRequest(r, token, "127.0.0.1:5555"); w.Code != http.StatusUnauthorized {
		t.Fatalf("second use of the same token must be 401 (single use), got %d", w.Code)
	}
}

func TestReplay_ForgedOrUnknownTokenRejected(t *testing.T) {
	r, gdb := newReplayTestEnv(t)
	seedReplayKey(t, gdb, 1, "victim-key", 1, nil)

	if w := replayRequest(r, "deadbeef", "127.0.0.1:5555"); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged token must be 401, got %d", w.Code)
	}
	// A token minted for a nonexistent key redeems fine but resolves to no key.
	if w := replayRequest(r, MintReplayToken(999), "127.0.0.1:5555"); w.Code != http.StatusUnauthorized {
		t.Fatalf("token for unknown key must be 401, got %d", w.Code)
	}
}

func TestReplay_ExpiredAndDisabledKeysRejected(t *testing.T) {
	r, gdb := newReplayTestEnv(t)
	past := time.Now().Add(-time.Hour)
	seedReplayKey(t, gdb, 2, "expired-key", 1, &past) // status 1, already expired
	seedReplayKey(t, gdb, 3, "disabled-key", 2, nil)

	if w := replayRequest(r, MintReplayToken(2), "127.0.0.1:5555"); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired key must not authenticate via replay, got %d", w.Code)
	}
	if w := replayRequest(r, MintReplayToken(3), "127.0.0.1:5555"); w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled key must not authenticate via replay, got %d", w.Code)
	}
}

func TestReplay_NonLoopbackSourceRejected(t *testing.T) {
	r, gdb := newReplayTestEnv(t)
	seedReplayKey(t, gdb, 1, "victim-key", 1, nil)

	// Even a genuinely minted token must not redeem from a non-loopback
	// source (isLocalhost stays as defense-in-depth behind the token).
	if w := replayRequest(r, MintReplayToken(1), "203.0.113.9:5555"); w.Code != http.StatusUnauthorized {
		t.Fatalf("non-loopback source must be 401, got %d", w.Code)
	}
}

func TestReplay_TTLExpiry(t *testing.T) {
	r, gdb := newReplayTestEnv(t)
	seedReplayKey(t, gdb, 1, "victim-key", 1, nil)

	token := MintReplayToken(1)
	// White-box: force the entry past its TTL, then redeem.
	replayTokens.mu.Lock()
	for k, e := range replayTokens.entries {
		e.expiresAt = time.Now().Add(-time.Second)
		replayTokens.entries[k] = e
	}
	replayTokens.mu.Unlock()

	if w := replayRequest(r, token, "127.0.0.1:5555"); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired token must be 401, got %d", w.Code)
	}
}

// The reporter's XFF control under the production trust configuration: with
// SetTrustedProxies(nil) a spoofed X-Forwarded-For: 127.0.0.1 must NOT turn a
// remote request into a "local" one — not even with a genuinely minted token.
func TestReplay_XFFForgeryIneffectiveUnderProductionTrustConfig(t *testing.T) {
	r, gdb := newReplayTestEnv(t)
	seedReplayKey(t, gdb, 1, "victim-key", 1, nil)

	token := MintReplayToken(1)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Replay-Token", token)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.RemoteAddr = "203.0.113.9:5555" // external client claiming to be local
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("XFF forgery must stay 401 under SetTrustedProxies(nil), got %d", w.Code)
	}
}
