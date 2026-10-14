package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSsrfAllowlistHandler(t *testing.T) (*SsrfAllowlistHandler, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SsrfAllowlistConfig{}, &model.AuditLog{}))
	t.Cleanup(func() { guardrail.ApplyInternalAllowlist(nil) })
	svc := service.NewSsrfAllowlistService(db, nil, []string{"10.1.0.0/16"}, true)
	svc.LoadColdStart(context.Background())
	// Page writes REQUIRE Redis-backed re-auth state (multi-instance
	// constraint is enforced by refusal, not documentation), so the default
	// harness runs in Redis mode over miniredis.
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewSsrfAllowlistHandler(svc, NewReauthManager(rdb), nil), db
}

// mintReauth issues a re-auth token directly from the manager for the given
// SESSION (Authorization header value), bypassing the password path (the
// manager's own tests cover that; here we exercise the write endpoints'
// enforcement including the session binding).
func mintReauth(t *testing.T, h *SsrfAllowlistHandler, userID int64, authHeader string) string {
	t.Helper()
	tok, err := h.reauth.Issue(userID, SessionFingerprint(authHeader), "pw", func(string) bool { return true })
	require.NoError(t, err)
	return tok
}

// withSession sets the Authorization header a real authenticated request
// would carry — the re-auth session fingerprint is derived from it.
func withSession(c *gin.Context, authHeader string) *gin.Context {
	c.Request.Header.Set("Authorization", authHeader)
	return c
}

func TestSsrfAllowlistHandler_Get(t *testing.T) {
	h, _ := setupSsrfAllowlistHandler(t)
	c, w := newTestContext(t, http.MethodGet, "/admin/api/system/ssrf-allowlist", nil)
	setAdminContext(c, 1, 1, "admin")
	h.Get(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"source":"env"`)
	assert.Contains(t, w.Body.String(), `"fallback_cidrs":["10.1.0.0/16"]`)
	assert.Contains(t, w.Body.String(), `"version":0`)
	assert.Contains(t, w.Body.String(), `"consistency"`)
}

func TestSsrfAllowlistHandler_Put_PermissionDenied(t *testing.T) {
	h, _ := setupSsrfAllowlistHandler(t)

	t.Run("org admin forbidden", func(t *testing.T) {
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
		setAdminContext(c, 5, 2, "org_admin")
		h.Put(c)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Contains(t, w.Body.String(), "platform admin")
	})

	t.Run("regular user forbidden", func(t *testing.T) {
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
		setAdminContext(c, 6, 3, "user")
		h.Put(c)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("delete also requires platform admin", func(t *testing.T) {
		c, w := newTestContext(t, http.MethodDelete, "/admin/api/system/ssrf-allowlist", gin.H{"expected_version": 0})
		setAdminContext(c, 5, 2, "org_admin")
		h.Delete(c)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// ── re-authentication enforcement on writes ──

func TestSsrfAllowlistHandler_WritesRequireReauth(t *testing.T) {
	h, _ := setupSsrfAllowlistHandler(t)

	t.Run("put without token rejected", func(t *testing.T) {
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
		setAdminContext(c, 1, 1, "admin")
		h.Put(c)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), ErrReauthTokenCode)
		// Nothing was written: the policy is still the static env seed.
		prefixes := guardrail.InternalAllowlistSnapshot()
		require.Len(t, prefixes, 1)
		assert.Equal(t, "10.1.0.0/16", prefixes[0].String())
	})

	t.Run("delete without token rejected", func(t *testing.T) {
		c, w := newTestContext(t, http.MethodDelete, "/admin/api/system/ssrf-allowlist", gin.H{"expected_version": 0})
		setAdminContext(c, 1, 1, "admin")
		h.Delete(c)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("token is single-use", func(t *testing.T) {
		tok := mintReauth(t, h, 1, "Bearer e2e-session-token")
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
		setAdminContext(c, 1, 1, "admin")
		c.Request.Header.Set("X-Reauth-Token", tok)
		c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
		h.Put(c)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		// Replaying the SAME token must fail (single use).
		c2, w2 := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.40.0.0/16"}, "expected_version": 1})
		setAdminContext(c2, 1, 1, "admin")
		c2.Request.Header.Set("X-Reauth-Token", tok)
		c2.Request.Header.Set("Authorization", "Bearer e2e-session-token")
		h.Put(c2)
		assert.Equal(t, http.StatusUnauthorized, w2.Code)
	})

	t.Run("token bound to the issuing user", func(t *testing.T) {
		tok := mintReauth(t, h, 1, "Bearer e2e-session-token")
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
		setAdminContext(c, 2, 1, "admin") // DIFFERENT platform admin, SAME session
		c.Request.Header.Set("X-Reauth-Token", tok)
		c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
		h.Put(c)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("token bound to the issuing SESSION (same user, other login)", func(t *testing.T) {
		tok := mintReauth(t, h, 1, "Bearer e2e-session-token")
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
		setAdminContext(c, 1, 1, "admin")                           // SAME user
		c.Request.Header.Set("Authorization", "Bearer other-login") // DIFFERENT session
		c.Request.Header.Set("X-Reauth-Token", tok)
		h.Put(c)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		// The failed cross-session attempt BURNED the token: the legitimate
		// session cannot use it either.
		c2, w2 := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
		setAdminContext(c2, 1, 1, "admin")
		c2.Request.Header.Set("Authorization", "Bearer e2e-session-token")
		c2.Request.Header.Set("X-Reauth-Token", tok)
		h.Put(c2)
		assert.Equal(t, http.StatusUnauthorized, w2.Code)
	})

	t.Run("re-login (new access token) kills old tokens", func(t *testing.T) {
		tok := mintReauth(t, h, 1, "Bearer old-login-token")
		// Logout + login again: a NEW access token ⇒ new session fingerprint.
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
		setAdminContext(c, 1, 1, "admin")
		c.Request.Header.Set("Authorization", "Bearer new-login-token")
		c.Request.Header.Set("X-Reauth-Token", tok)
		h.Put(c)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestSsrfAllowlistHandler_Put_AndApply(t *testing.T) {
	h, db := setupSsrfAllowlistHandler(t)

	// Admin applies a list.
	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{
		"cidrs": []string{" 10.30.0.0/16 "}, "expected_version": 0,
	})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Put(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"version":1`)
	assert.Contains(t, w.Body.String(), `"effective_cidrs":["10.30.0.0/16"]`) // trimmed + canonical

	// Memory policy published.
	prefixes := guardrail.InternalAllowlistSnapshot()
	require.Len(t, prefixes, 1)
	assert.Equal(t, "10.30.0.0/16", prefixes[0].String())

	// Same-transaction audit row carries the operator.
	var audit model.AuditLog
	require.NoError(t, db.First(&audit).Error)
	assert.Equal(t, "system:update_ssrf_allowlist", audit.Action)
	assert.Equal(t, "test-user", audit.Username)
	assert.Equal(t, int64(1), audit.UserID)
}

func TestSsrfAllowlistHandler_Put_MissingCIDRsVsExplicitEmpty(t *testing.T) {
	h, db := setupSsrfAllowlistHandler(t)

	// Field absent → rejected with guidance distinguishing the two ops.
	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"expected_version": 0})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Put(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "explicit empty array")
	assert.Contains(t, w.Body.String(), "ssrf_allowlist_invalid")

	// Explicit [] → valid strict OVERRIDE (row present, env not applied).
	c, w = newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{}, "expected_version": 0})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Put(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var row model.SsrfAllowlistConfig
	require.NoError(t, db.First(&row).Error)
	assert.Equal(t, "[]", row.CIDRsJSON)
	assert.False(t, row.Removed)
	assert.Empty(t, guardrail.InternalAllowlistSnapshot())
}

func TestSsrfAllowlistHandler_Put_InvalidAndConflict(t *testing.T) {
	h, _ := setupSsrfAllowlistHandler(t)

	// Invalid CIDR: 400 with the shared validator's reason; nothing applied.
	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"8.8.8.8/32"}, "expected_version": 0})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Put(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "not a subnet of a private range")

	// Create version 1, then replay a stale expected_version.
	c, w = newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Put(c)
	require.Equal(t, http.StatusOK, w.Code)

	c, w = newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.40.0.0/16"}, "expected_version": 0})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Put(c)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), `"current_version":1`)
	assert.Contains(t, w.Body.String(), `"current_cidrs":["10.30.0.0/16"]`)
}

func TestSsrfAllowlistHandler_Delete_ResetSemantics(t *testing.T) {
	h, db := setupSsrfAllowlistHandler(t)

	// Seed override v1.
	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Put(c)
	require.Equal(t, http.StatusOK, w.Code)

	// Reset: falls back to env; row is TOMBSTONED at v2 (kept, not deleted).
	c, w = newTestContext(t, http.MethodDelete, "/admin/api/system/ssrf-allowlist", gin.H{"expected_version": 1})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Delete(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"source":"env"`)
	var row model.SsrfAllowlistConfig
	require.NoError(t, db.First(&row).Error)
	assert.True(t, row.Removed)
	assert.Equal(t, int64(2), row.Version)
	require.Len(t, guardrail.InternalAllowlistSnapshot(), 1) // env value active

	// Stale reset → conflict (version advanced to 2).
	c, w = newTestContext(t, http.MethodDelete, "/admin/api/system/ssrf-allowlist", gin.H{"expected_version": 1})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Delete(c)
	assert.Equal(t, http.StatusConflict, w.Code)

	// No-op reset (tombstone at v2, expected 2) → success.
	c, w = newTestContext(t, http.MethodDelete, "/admin/api/system/ssrf-allowlist", gin.H{"expected_version": 2})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Delete(c)
	assert.Equal(t, http.StatusOK, w.Code)

	// Reset via ?expected_version= query (HTTP clients without DELETE bodies).
	c, w = newTestContext(t, http.MethodDelete, "/admin/api/system/ssrf-allowlist", nil)
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	h.Delete(c)
	// no query/body → expected 0 with an EXISTING row → conflict
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestSsrfAllowlistHandler_AuditFields_NoSecretLeak(t *testing.T) {
	h, db := setupSsrfAllowlistHandler(t)
	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
	setAdminContext(c, 1, 1, "admin")
	c.Request.Header.Set("X-Reauth-Token", mintReauth(t, h, 1, "Bearer e2e-session-token"))
	c.Request.Header.Set("Authorization", "Bearer e2e-session-token")
	c.Request.Header.Set("User-Agent", "ua-test")
	h.Put(c)
	require.Equal(t, http.StatusOK, w.Code)
	var audit model.AuditLog
	require.NoError(t, db.First(&audit).Error)
	assert.Equal(t, "ua-test", audit.UserAgent)
	assert.Equal(t, int64(1), audit.UserID)
	assert.Contains(t, string(audit.Detail), `"version":1`)
	assert.NotContains(t, string(audit.Detail), "api_key")
	assert.NotContains(t, string(audit.Detail), "password")
	assert.False(t, audit.CreatedAt.IsZero())
}

// ── ReauthManager unit behavior (session binding / atomic rate limit / TTL) ──

const fpA = "fingerprint-session-a"
const fpB = "fingerprint-session-b"

func TestReauthManager_Lifecycle(t *testing.T) {
	m := NewReauthManager(nil)
	base := time.Unix(1700000000, 0)
	m.now = func() time.Time { return base }
	ok := func(string) bool { return true }

	t.Run("wrong password rejected, no token", func(t *testing.T) {
		_, err := m.Issue(1, fpA, "wrong", func(p string) bool { return p == "right" })
		require.ErrorIs(t, err, ErrReauthInvalid)
	})

	t.Run("rate limit after repeated failures; window expiry clears", func(t *testing.T) {
		for i := 0; i < reauthMaxFails-1; i++ { // 1 failure already above
			_, _ = m.Issue(1, fpA, "wrong", func(string) bool { return false })
		}
		_, err := m.Issue(1, fpA, "right", ok)
		require.ErrorIs(t, err, ErrReauthRateLimited)
		// Other subjects are unaffected.
		_, err = m.Issue(2, fpA, "right", ok)
		require.NoError(t, err)
		// Window expiry clears the limiter.
		m.now = func() time.Time { return base.Add(reauthFailWindow + time.Second) }
		_, err = m.Issue(1, fpA, "right", ok)
		require.NoError(t, err)
	})

	t.Run("token: single-use, expires, user- and session-bound", func(t *testing.T) {
		m.now = func() time.Time { return base }
		tok, err := m.Issue(7, fpA, "right", ok)
		require.NoError(t, err)
		require.NoError(t, m.Verify(7, fpA, tok))                    // ok, consumed
		require.ErrorIs(t, m.Verify(7, fpA, tok), ErrReauthTokenBad) // single-use
		tok2, err := m.Issue(7, fpA, "right", ok)
		require.NoError(t, err)
		require.ErrorIs(t, m.Verify(8, fpA, tok2), ErrReauthTokenBad) // other user
		tok3, err := m.Issue(7, fpA, "right", ok)
		require.NoError(t, err)
		require.ErrorIs(t, m.Verify(7, fpB, tok3), ErrReauthTokenBad) // other session of SAME user
		m.now = func() time.Time { return base.Add(reauthTokenTTL + time.Second) }
		tok4, err := m.Issue(7, fpA, "right", ok)
		require.NoError(t, err)
		m.now = func() time.Time { return base.Add(3 * reauthTokenTTL) }
		require.ErrorIs(t, m.Verify(7, fpA, tok4), ErrReauthTokenBad) // expired
	})
}

// CONCURRENCY counter-example: the attempt slot is reserved BEFORE the
// (slow) verification, so concurrent issues cannot all slip past the limit —
// the number of actual verifications is bounded by reauthMaxFails per window.
func TestReauthManager_ConcurrentIssue_RateLimitNotBypassed(t *testing.T) {
	m := NewReauthManager(nil)
	var verifications int64
	var mu sync.Mutex
	verify := func(string) bool {
		mu.Lock()
		verifications++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // widen the race window
		return false
	}
	const concurrent = 20
	var wg sync.WaitGroup
	rateLimited := int64(0)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Issue(42, fpA, "pw", verify); err == ErrReauthRateLimited {
				atomic.AddInt64(&rateLimited, 1)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	v := verifications
	mu.Unlock()
	require.LessOrEqual(t, v, int64(reauthMaxFails), "concurrent issues bypassed the attempt limit: %d verifications ran", v)
	require.EqualValues(t, concurrent-v, rateLimited, "every non-verified attempt must get the fixed rate-limit response")
	// After the storm the subject is fully locked out.
	_, err := m.Issue(42, fpA, "right", func(string) bool { return true })
	require.ErrorIs(t, err, ErrReauthRateLimited)
}

// Redis mode: SHARED state across replicas with ATOMIC consume and limits.
func TestReauthManager_RedisSharedState(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	nodeA := NewReauthManager(rdb)
	nodeB := NewReauthManager(rdb) // a different replica
	ok := func(string) bool { return true }
	require.Equal(t, "redis", nodeA.Mode())

	t.Run("token minted on A is consumable exactly once on B", func(t *testing.T) {
		tok, err := nodeA.Issue(9, fpA, "right", ok)
		require.NoError(t, err)
		require.NoError(t, nodeB.Verify(9, fpA, tok)) // cross-replica, same session
		require.ErrorIs(t, nodeA.Verify(9, fpA, tok), ErrReauthTokenBad)
	})

	t.Run("same user other session rejected cross-replica", func(t *testing.T) {
		tok, err := nodeA.Issue(9, fpA, "right", ok)
		require.NoError(t, err)
		require.ErrorIs(t, nodeB.Verify(9, fpB, tok), ErrReauthTokenBad)
	})

	t.Run("concurrent Verify of one token: exactly one winner", func(t *testing.T) {
		tok, err := nodeA.Issue(9, fpA, "right", ok)
		require.NoError(t, err)
		const racers = 8
		var wg sync.WaitGroup
		var wins int64
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := nodeA
				if i%2 == 1 {
					m = nodeB
				}
				if err := m.Verify(9, fpA, tok); err == nil {
					atomic.AddInt64(&wins, 1)
				}
			}(i)
		}
		wg.Wait()
		require.EqualValues(t, 1, wins, "GETDEL must yield exactly one winner")
	})

	t.Run("failure counter shared across replicas", func(t *testing.T) {
		for i := 0; i < reauthMaxFails; i++ {
			_, _ = nodeA.Issue(77, fpA, "wrong", func(string) bool { return false })
		}
		_, err := nodeB.Issue(77, fpA, "right", ok) // B sees A's failures
		require.ErrorIs(t, err, ErrReauthRateLimited)
	})

	t.Run("concurrent issues cannot bypass the shared limit", func(t *testing.T) {
		var verifications int64
		var mu sync.Mutex
		verify := func(string) bool {
			mu.Lock()
			verifications++
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			return false
		}
		const concurrent = 12
		var wg sync.WaitGroup
		for i := 0; i < concurrent; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := nodeA
				if i%2 == 1 {
					m = nodeB
				}
				_, _ = m.Issue(88, fpA, "pw", verify)
			}(i)
		}
		wg.Wait()
		mu.Lock()
		v := verifications
		mu.Unlock()
		require.LessOrEqual(t, v, int64(reauthMaxFails), "shared counter bypassed: %d verifications", v)
	})

	t.Run("redis down fails closed", func(t *testing.T) {
		mr.Close()
		_, err := nodeA.Issue(99, fpA, "right", ok)
		require.ErrorIs(t, err, ErrReauthUnavailable)
		require.ErrorIs(t, nodeA.Verify(99, fpA, "any-token"), ErrReauthUnavailable)
	})
}

// Reauth endpoint shape: wrong password → 401 with a FIXED message that
// never echoes the credential; success mints a token usable on writes.
func TestSsrfAllowlistHandler_ReauthEndpoint(t *testing.T) {
	h, _ := setupSsrfAllowlistHandler(t)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/system/ssrf-allowlist/reauth", gin.H{"password": "super-secret-pw"})
	setAdminContext(c, 1, 1, "admin")
	h.Reauth(c)
	assert.Equal(t, http.StatusUnauthorized, w.Code) // users=nil in tests → verify fails
	// Fixed message; the credential never appears anywhere in the response.
	assert.NotContains(t, w.Body.String(), "super-secret-pw")
	assert.Contains(t, w.Body.String(), ErrReauthInvalidCode)

	// org_admin cannot mint tokens at all.
	c, w = newTestContext(t, http.MethodPost, "/admin/api/system/ssrf-allowlist/reauth", gin.H{"password": "x"})
	setAdminContext(c, 5, 2, "org_admin")
	h.Reauth(c)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ── v4: per-login JTI — same-second/concurrent logins mint DISTINCT tokens ──

func TestGenerateToken_UniqueSessionIDPerLogin(t *testing.T) {
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)
	cfg := config.AdminConfig{TokenExpiry: 24, JWTSecret: "unit-test-secret-0123456789abcdef"}
	user := &model.User{ID: 1, Username: "admin", RoleID: 1}

	// Consecutive, same second.
	a, err := GenerateToken(user, "admin", 0, 0, "", cfg, cp)
	require.NoError(t, err)
	b, err := GenerateToken(user, "admin", 0, 0, "", cfg, cp)
	require.NoError(t, err)
	require.NotEqual(t, a, b, "two logins in the same second must not mint identical JWTs")

	// Concurrent burst — every token distinct.
	const n = 24
	tokens := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := GenerateToken(user, "admin", 0, 0, "", cfg, cp)
			require.NoError(t, err)
			tokens[i] = tok
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, tok := range tokens {
		require.False(t, seen[tok], "duplicate JWT in concurrent login burst")
		seen[tok] = true
	}
}

// Real-chain session binding via the middleware-provided session_id (jti):
// A's re-auth token cannot be consumed by B (a second login), A consumes it
// exactly once.
func TestSsrfAllowlistHandler_JTI_SessionBinding(t *testing.T) {
	h, _ := setupSsrfAllowlistHandler(t)

	// Two "logins" → two distinct jtis (as JWTAuthMiddleware would set).
	login := func(sid string) (string, *gin.Context, *httptest.ResponseRecorder) {
		c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
		setAdminContext(c, 1, 1, "admin")
		c.Set("session_id", sid)
		c.Request.Header.Set("X-Requested-With", "XMLHttpRequest")
		return sid, c, w
	}
	sessA, ctxA, wA := login("jti-aaaaaaaa")
	_, ctxB, wB := login("jti-bbbbbbbb")

	rt, err := h.reauth.Issue(1, "jti:"+sessA, "pw", func(string) bool { return true })
	require.NoError(t, err)

	// B (same user, different login) cannot consume A's token.
	ctxB.Request.Header.Set("X-Reauth-Token", rt)
	h.Put(ctxB)
	require.Equal(t, http.StatusUnauthorized, wB.Code, wB.Body.String())

	// Burned by the failed attempt: A cannot use it either.
	ctxA2, wA2 := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
	setAdminContext(ctxA2, 1, 1, "admin")
	ctxA2.Set("session_id", sessA)
	ctxA2.Request.Header.Set("X-Reauth-Token", rt)
	h.Put(ctxA2)
	assert.Equal(t, http.StatusUnauthorized, wA2.Code)

	// A re-issues and consumes exactly once: first use OK, replay 401.
	rt2, err := h.reauth.Issue(1, "jti:"+sessA, "pw", func(string) bool { return true })
	require.NoError(t, err)
	ctxA.Request.Header.Set("X-Reauth-Token", rt2)
	h.Put(ctxA)
	require.Equal(t, http.StatusOK, wA.Code, wA.Body.String())
	ctxA3, wA3 := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.31.0.0/16"}, "expected_version": 1})
	setAdminContext(ctxA3, 1, 1, "admin")
	ctxA3.Set("session_id", sessA)
	ctxA3.Request.Header.Set("X-Reauth-Token", rt2)
	h.Put(ctxA3)
	assert.Equal(t, http.StatusUnauthorized, wA3.Code)
}

// Without Redis the page write path is REFUSED outright (enforceable
// single-instance constraint: no per-node tokens/limits to bypass) and the
// status API reports reauth:"local".
func TestSsrfAllowlistHandler_LocalMode_RefusesWrites(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SsrfAllowlistConfig{}, &model.AuditLog{}))
	t.Cleanup(func() { guardrail.ApplyInternalAllowlist(nil) })
	svc := service.NewSsrfAllowlistService(db, nil, nil, false)
	svc.LoadColdStart(context.Background())
	h := NewSsrfAllowlistHandler(svc, NewReauthManager(nil), nil)

	c, w := newTestContext(t, http.MethodPut, "/admin/api/system/ssrf-allowlist", gin.H{"cidrs": []string{"10.30.0.0/16"}, "expected_version": 0})
	setAdminContext(c, 1, 1, "admin")
	c.Set("session_id", "jti-x")
	h.Put(c)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), ErrReauthRedisRequiredCode)
	assert.Contains(t, w.Body.String(), "Redis")

	c2, w2 := newTestContext(t, http.MethodPost, "/admin/api/system/ssrf-allowlist/reauth", gin.H{"password": "x"})
	setAdminContext(c2, 1, 1, "admin")
	h.Reauth(c2)
	assert.Equal(t, http.StatusConflict, w2.Code)

	c3, w3 := newTestContext(t, http.MethodDelete, "/admin/api/system/ssrf-allowlist", gin.H{"expected_version": 0})
	setAdminContext(c3, 1, 1, "admin")
	h.Delete(c3)
	assert.Equal(t, http.StatusConflict, w3.Code)

	// Status explicitly reports the local mode for the UI to surface.
	c4, w4 := newTestContext(t, http.MethodGet, "/admin/api/system/ssrf-allowlist", nil)
	setAdminContext(c4, 1, 1, "admin")
	h.Get(c4)
	assert.Contains(t, w4.Body.String(), `"reauth":"local"`)

	// Nothing was written: memory policy unchanged (strict).
	assert.Empty(t, guardrail.InternalAllowlistSnapshot())
}

// ── v4: Redis limiter evidence — TTL always armed, no-TTL keys repaired,
// window expiry starts fresh, success resets the window ──

func TestReauthManager_RedisLimiter_TTLAndRepair(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	m := NewReauthManager(rdb)
	ctx := context.Background()
	key := reauthFailKey(5)

	// After failures the counter ALWAYS carries a TTL (no stranded keys).
	for i := 0; i < 3; i++ {
		_, _ = m.Issue(5, fpA, "wrong", func(string) bool { return false })
	}
	ttl, err := rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, reauthFailWindow)

	// Simulate the legacy two-command gap: a counter stranded WITHOUT TTL.
	// The next claim repairs it (arms the window) instead of locking the
	// subject out forever.
	require.NoError(t, rdb.Set(ctx, key, 3, 0).Err())
	_, err = m.Issue(5, fpA, "wrong", func(string) bool { return false })
	require.ErrorIs(t, err, ErrReauthInvalid)
	ttl, err = rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0), "no-TTL counter must be repaired with a window")
	v, err := rdb.Get(ctx, key).Int()
	require.NoError(t, err)
	require.Equal(t, 4, v)

	// Window expiry starts the next claim at 1 with a fresh TTL.
	require.NoError(t, rdb.Del(ctx, key).Err())
	_, _ = m.Issue(5, fpA, "wrong", func(string) bool { return false })
	v, err = rdb.Get(ctx, key).Int()
	require.NoError(t, err)
	require.Equal(t, 1, v)

	// A successful re-auth clears the window entirely: a subsequent failure
	// counts from 1 again (reset semantics, not accumulation).
	_, err = m.Issue(5, fpA, "right", func(string) bool { return true })
	require.NoError(t, err)
	gone, err := rdb.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, gone)
	_, _ = m.Issue(5, fpA, "wrong", func(string) bool { return false })
	v, err = rdb.Get(ctx, key).Int()
	require.NoError(t, err)
	require.Equal(t, 1, v)
}
