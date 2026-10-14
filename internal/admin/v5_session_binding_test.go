package admin

// v5 review counter-examples: session binding must follow the credential the
// REAL auth middleware verified (cookie first, else bearer), and the
// near-expiry auto-refresh must keep the login's session identity stable.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func v5TestAuth(t *testing.T) (config.AdminConfig, crypto.CryptoProvider) {
	t.Helper()
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)
	return config.AdminConfig{TokenExpiry: 24, JWTSecret: "unit-test-secret-0123456789abcdef"}, cp
}

func v5SignToken(t *testing.T, cp crypto.CryptoProvider, secret string, claims Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(cp.JWTSigningMethod(), claims).SignedString([]byte(secret))
	require.NoError(t, err)
	return s
}

func v5ParseToken(t *testing.T, secret, token string) *Claims {
	t.Helper()
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(tk *jwt.Token) (any, error) {
		return []byte(secret), nil
	})
	require.NoError(t, err)
	require.True(t, parsed.Valid)
	return claims
}

// v5LegacyLogin mints a LEGACY token (no jti) — the pre-B5-v4 shape that
// used to be indistinguishable across cookie-only logins. iatSkew lets the
// caller emulate two logins in DIFFERENT seconds (real distinct legacy
// credentials); same-second legacy logins mint the identical token, which is
// precisely the pre-jti weakness v4 fixed at GenerateToken level.
func v5LegacyLogin(t *testing.T, cp crypto.CryptoProvider, secret string, iatSkew time.Duration) string {
	t.Helper()
	now := time.Now().Add(iatSkew)
	return v5SignToken(t, cp, secret, Claims{
		UserID: 1, Username: "admin", RoleID: 1, RoleName: model.RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "crosslink",
		},
	})
}

// v5MiddlewarePut runs the REAL JWTAuthMiddleware over a PUT carrying the
// given cookie / Authorization header, exactly as production routing would,
// then leaves the context ready for the handler under test.
func v5MiddlewarePut(t *testing.T, cfg config.AdminConfig, cp crypto.CryptoProvider, cookie, authz, reauthTok string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/admin/api/system/ssrf-allowlist",
		strings.NewReader(`{"cidrs":["10.30.0.0/16"],"expected_version":0}`))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "admin_token", Value: cookie})
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if reauthTok != "" {
		req.Header.Set("X-Reauth-Token", reauthTok)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	JWTAuthMiddleware(cfg, nil, cp)(c)
	return c, w
}

// Cookie-only LEGACY logins (no jti): two different cookies of the same
// user must NOT collapse onto SHA256("") — each binds to its own verified
// credential, so one session's re-auth token is unusable in the other.
func TestSessionBinding_RealMiddleware_CookieLegacyLogins(t *testing.T) {
	h, _ := setupSsrfAllowlistHandler(t)
	cfg, cp := v5TestAuth(t)
	tokenA, tokenB := v5LegacyLogin(t, cp, cfg.JWTSecret, 0), v5LegacyLogin(t, cp, cfg.JWTSecret, time.Second)
	require.NotEqual(t, tokenA, tokenB, "two legacy logins must mint distinct credentials")

	cA, _ := v5MiddlewarePut(t, cfg, cp, tokenA, "", "")
	cB, _ := v5MiddlewarePut(t, cfg, cp, tokenB, "", "")
	fpA, fpB := h.sessionFingerprint(cA), h.sessionFingerprint(cB)
	emptyHash := SessionFingerprint("")
	require.NotEqual(t, fpA, fpB)
	require.NotEqual(t, fpA, emptyHash, "cookie-only legacy session must not collapse to SHA256(empty)")
	require.Equal(t, "cred:"+SessionFingerprint(tokenA), fpA)

	// A's re-auth token is bound to A's cookie; B cannot consume it (the
	// failed attempt also BURNS it — single use). A fresh token works for A.
	rt, err := h.reauth.Issue(1, fpA, "pw", func(string) bool { return true })
	require.NoError(t, err)
	cB2, wB2 := v5MiddlewarePut(t, cfg, cp, tokenB, "", rt)
	h.Put(cB2)
	assert.Equal(t, http.StatusUnauthorized, wB2.Code)

	rt2, err := h.reauth.Issue(1, fpA, "pw", func(string) bool { return true })
	require.NoError(t, err)
	cA2, wA2 := v5MiddlewarePut(t, cfg, cp, tokenA, "", rt2)
	h.Put(cA2)
	assert.Equal(t, http.StatusOK, wA2.Code, wA2.Body.String())
}

// Cookie/header conflict: with a valid admin_token cookie the middleware
// authenticated the COOKIE — the Authorization header is not the verified
// credential and must not define (or shift) the session identity.
func TestSessionBinding_RealMiddleware_CookieHeaderConflict(t *testing.T) {
	h, _ := setupSsrfAllowlistHandler(t)
	cfg, cp := v5TestAuth(t)
	tokenA, tokenB := v5LegacyLogin(t, cp, cfg.JWTSecret, 0), v5LegacyLogin(t, cp, cfg.JWTSecret, time.Second)

	// Cookie A is the verified credential; the header carries B's token.
	cC, _ := v5MiddlewarePut(t, cfg, cp, tokenA, "Bearer "+tokenB, "")
	fpC := h.sessionFingerprint(cC)
	assert.Equal(t, "cred:"+SessionFingerprint(tokenA), fpC, "identity must follow the cookie the middleware verified")
	assert.NotEqual(t, SessionFingerprint("Bearer "+tokenB), fpC)

	// A re-auth token minted in that conflicted context is consumable ONLY
	// by sessions verified with cookie A — not by a session whose verified
	// credential is B (even when B's header matches its own cookie).
	rt, err := h.reauth.Issue(1, fpC, "pw", func(string) bool { return true })
	require.NoError(t, err)
	cB, wB := v5MiddlewarePut(t, cfg, cp, tokenB, "Bearer "+tokenB, rt)
	h.Put(cB)
	assert.Equal(t, http.StatusUnauthorized, wB.Code)
	// Fresh token (the one above was burned by B's failed attempt).
	rt2, err := h.reauth.Issue(1, fpC, "pw", func(string) bool { return true })
	require.NoError(t, err)
	cA, wA := v5MiddlewarePut(t, cfg, cp, tokenA, "", rt2)
	h.Put(cA)
	assert.Equal(t, http.StatusOK, wA.Code, wA.Body.String())
}

// Near-expiry auto-refresh keeps the SAME login session identity: the
// refreshed token reuses the current jti, so a re-auth token issued just
// before the refresh survives the browser adopting the refreshed cookie.
// Full chain: near-expiry login → refresh → re-auth → save → replay-fail.
func TestSessionBinding_RealMiddleware_RefreshPreservesSession(t *testing.T) {
	h, _ := setupSsrfAllowlistHandler(t)
	cfg, cp := v5TestAuth(t)
	now := time.Now()
	const sid = "sess-identity-kept-across-refresh"

	nearExpiry := v5SignToken(t, cp, cfg.JWTSecret, Claims{
		UserID: 1, Username: "admin", RoleID: 1, RoleName: model.RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        sid,
			ExpiresAt: jwt.NewNumericDate(now.Add(3 * time.Minute)), // inside the 5-minute refresh window
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "crosslink",
		},
	})

	// Request through the middleware with the near-expiry cookie: the
	// current request keeps the session identity AND a refresh is issued.
	cOld, wOld := v5MiddlewarePut(t, cfg, cp, nearExpiry, "", "")
	require.Equal(t, "jti:"+sid, h.sessionFingerprint(cOld))
	refreshed := wOld.Header().Get("X-Token-Refresh")
	require.NotEmpty(t, refreshed, "near-expiry token must trigger auto-refresh")
	rClaims := v5ParseToken(t, cfg.JWTSecret, refreshed)
	require.Equal(t, sid, rClaims.ID, "refreshed token must keep the login's jti")
	require.True(t, rClaims.ExpiresAt.After(now.Add(20*time.Hour)), "refreshed token must extend expiry")

	// NEXT request with the refreshed cookie: identity unchanged, so a
	// re-auth token minted pre- (or post-) refresh is consumable either way.
	cNew, _ := v5MiddlewarePut(t, cfg, cp, refreshed, "", "")
	require.Equal(t, "jti:"+sid, h.sessionFingerprint(cNew))

	// Mint the re-auth token against the PRE-refresh identity, then consume
	// it with the POST-refresh credential — exactly the reviewer's scenario.
	rt, err := h.reauth.Issue(1, h.sessionFingerprint(cOld), "pw", func(string) bool { return true })
	require.NoError(t, err)
	cSave, wSave := v5MiddlewarePut(t, cfg, cp, refreshed, "", rt)
	h.Put(cSave)
	require.Equal(t, http.StatusOK, wSave.Code, wSave.Body.String())
	cReplay, wReplay := v5MiddlewarePut(t, cfg, cp, refreshed, "", rt)
	h.Put(cReplay)
	assert.Equal(t, http.StatusUnauthorized, wReplay.Code)

	// Legacy near-expiry tokens are UPGRADED: the refreshed token gains a
	// fresh jti (a stable identity from then on).
	legacyNear := v5LegacyLogin(t, cp, cfg.JWTSecret, 0)
	// Push it inside the refresh window by re-signing with a near expiry.
	legacyNear = v5SignToken(t, cp, cfg.JWTSecret, Claims{
		UserID: 1, Username: "admin", RoleID: 1, RoleName: model.RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(3 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "crosslink",
		},
	})
	_, wLegacy := v5MiddlewarePut(t, cfg, cp, legacyNear, "", "")
	refreshedLegacy := wLegacy.Header().Get("X-Token-Refresh")
	require.NotEmpty(t, refreshedLegacy)
	lClaims := v5ParseToken(t, cfg.JWTSecret, refreshedLegacy)
	require.NotEmpty(t, lClaims.ID, "legacy refresh must mint a jti (upgrade to stable identity)")
}
