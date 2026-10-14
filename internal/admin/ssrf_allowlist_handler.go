package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

// SsrfAllowlistHandler exposes the hot-reloadable SSRF internal allowlist
// (B5). Routes are gated by RequireAction (system:view for reads,
// system:update for writes) plus an explicit platform-admin check in the
// write handlers — tenant (org) admins must not change this GLOBAL policy.
// CSRF and session auth come from the adminGroup middleware chain.
//
// WRITE OPERATIONS REQUIRE RE-AUTHENTICATION: a short-lived, user-bound,
// single-use token from POST .../reauth (the operator re-enters their
// account password, which is verified with the same bcrypt path as login,
// never logged, never in a URL). The token is presented via the
// X-Reauth-Token header on PUT/DELETE. Rationale: a hijacked long-lived
// admin session must not be able to widen the internal-attack surface.
//
// The two write operations have deliberately distinct semantics that the API
// keeps separate end to end: PUT with an explicit empty array stores an
// override meaning "strictly deny all internal targets" (still overriding
// env/YAML), while DELETE tombstones the override (the row and its monotonic
// version are KEPT — anti-ABA) and falls back to env > YAML > strict.
type SsrfAllowlistHandler struct {
	svc    *service.SsrfAllowlistService
	reauth *ReauthManager
	users  *repository.UserRepo
}

func NewSsrfAllowlistHandler(svc *service.SsrfAllowlistService, reauth *ReauthManager, users *repository.UserRepo) *SsrfAllowlistHandler {
	return &SsrfAllowlistHandler{svc: svc, reauth: reauth, users: users}
}

// ssrfAllowlistActor captures the same context fields the async audit
// service uses; here they feed the SAME-TRANSACTION audit row.
func ssrfAllowlistActor(c *gin.Context) service.SsrfAllowlistActor {
	userID, _ := c.Get("user_id")
	username, _ := c.Get("username")
	id, _ := userID.(int64)
	name, _ := username.(string)
	return service.SsrfAllowlistActor{
		UserID:    id,
		Username:  name,
		IP:        c.ClientIP(),
		UserAgent: c.Request.UserAgent(),
	}
}

// Get reports the effective policy (memory truth), source, DB version,
// reset-fallback preview, degraded marker, consistency window, and where
// re-authentication state lives (redis = shared/atomic, local = node-only).
func (h *SsrfAllowlistHandler) Get(c *gin.Context) {
	st, _ := h.svc.Status(c.Request.Context())
	if st == nil {
		internalErr(c, errors.New("nil status"), "ssrf allowlist status failed")
		return
	}
	st.Consistency.Reauth = h.reauth.Mode()
	c.JSON(http.StatusOK, gin.H{"data": st})
}

// sessionFingerprint derives the session identity for re-auth binding. The
// auth middleware computes it from the credential it ACTUALLY verified
// (cookie first, else bearer): "jti:<per-login id>" for modern tokens,
// "cred:<sha256 of the raw credential>" for legacy ones. The fallbacks below
// exist only for direct handler invocation without the middleware (unit
// tests); on any production route the middleware value always wins, so an
// unverified Authorization header can never define the identity.
func (h *SsrfAllowlistHandler) sessionFingerprint(c *gin.Context) string {
	if fp, ok := c.Get("session_fingerprint"); ok {
		if s, ok := fp.(string); ok && s != "" {
			return s
		}
	}
	if sid, ok := c.Get("session_id"); ok {
		if s, ok := sid.(string); ok && s != "" {
			return "jti:" + s
		}
	}
	return SessionFingerprint(c.GetHeader("Authorization"))
}

// requireRedisBackedReauth enforces the multi-instance constraint: without
// shared (Redis) re-auth state, page writes are REFUSED outright — per-node
// tokens/limits would be bypassable across replicas, and silently accepting
// them is worse than declining the edit (env/YAML + restart still works).
func (h *SsrfAllowlistHandler) requireRedisBackedReauth(c *gin.Context) bool {
	if h.reauth.Mode() != "redis" {
		errorResp(c, http.StatusConflict, ErrReauthRedisRequiredCode,
			"page edits of the SSRF allowlist require Redis-backed shared re-authentication state; configure Redis, or edit env/YAML and restart")
		return false
	}
	return true
}

type ssrfReauthInput struct {
	// Password is the operator's CURRENT account password. It is verified
	// against the account's bcrypt hash and then discarded — never stored,
	// never logged, never echoed (fixed error messages only).
	Password string `json:"password"`
}

// Reauth mints a single-use, short-lived, user-bound token after verifying
// the operator's account password. Rate-limited per user on failures.
func (h *SsrfAllowlistHandler) Reauth(c *gin.Context) {
	if !IsAdmin(c) {
		errorResp(c, http.StatusForbidden, ErrInsufficientPermissions, "platform admin role required to change the SSRF allowlist")
		return
	}
	if !h.requireRedisBackedReauth(c) {
		return
	}
	var in ssrfReauthInput
	if err := c.ShouldBindJSON(&in); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	userID, _ := c.Get("user_id")
	uid, _ := userID.(int64)
	verify := func(password string) bool {
		if h.users == nil {
			return false
		}
		u, err := h.users.GetByID(context.Background(), uid)
		if err != nil || u == nil {
			// Dummy compare keeps timing uniform for unknown users.
			return verifyPassword(password, dummyBcryptHash)
		}
		return verifyPassword(password, u.PasswordHash)
	}
	token, err := h.reauth.Issue(uid, h.sessionFingerprint(c), in.Password, verify)
	if err != nil {
		switch {
		case errors.Is(err, ErrReauthRateLimited):
			errorResp(c, http.StatusTooManyRequests, ErrReauthRateLimitedCode, err.Error())
		case errors.Is(err, ErrReauthUnavailable):
			// Backing store unreachable: fail closed with its own code so the
			// UI can say "retry later" instead of "wrong password".
			errorResp(c, http.StatusServiceUnavailable, ErrReauthUnavailableCode, err.Error())
		default:
			// Fixed message only — no credential-derived content.
			errorResp(c, http.StatusUnauthorized, ErrReauthInvalidCode, err.Error())
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"token": token, "expires_in": int(reauthTokenTTL.Seconds())}})
}

// requireReauth enforces the re-authentication token header for writes. The
// token is bound to the ISSUING session (access-token fingerprint) and to
// the issuing user; another session of the same user cannot consume it.
func (h *SsrfAllowlistHandler) requireReauth(c *gin.Context) bool {
	userID, _ := c.Get("user_id")
	uid, _ := userID.(int64)
	if err := h.reauth.Verify(uid, h.sessionFingerprint(c), c.GetHeader("X-Reauth-Token")); err != nil {
		if errors.Is(err, ErrReauthUnavailable) {
			// Backing store unreachable: same fail-closed 503 as the issue
			// side, so a Redis outage reads as "retry later", not "bad token".
			errorResp(c, http.StatusServiceUnavailable, ErrReauthUnavailableCode, err.Error())
		} else {
			errorResp(c, http.StatusUnauthorized, ErrReauthTokenCode, err.Error())
		}
		return false
	}
	return true
}

type ssrfAllowlistPutInput struct {
	// CIDRs is a POINTER so "field absent" (rejected: ambiguous intent) and
	// "explicit empty array" (valid: enforce the strict policy as an
	// override) are distinguishable — the exact distinction B5 requires.
	CIDRs           *[]string `json:"cidrs"`
	ExpectedVersion int64     `json:"expected_version"`
}

func (h *SsrfAllowlistHandler) writeError(c *gin.Context, serr *service.SsrfAllowlistError) {
	switch serr.Kind {
	case service.SsrfErrInvalid:
		errorResp(c, http.StatusBadRequest, ErrSsrfAllowlistInvalid, serr.Message)
	case service.SsrfErrConflict:
		body := gin.H{
			"error":           "version conflict: the allowlist was changed by someone else; reload and retry",
			"error_code":      ErrConflict,
			"current_version": *serr.CurrentVersion,
		}
		if serr.CurrentCIDRs != nil {
			body["current_cidrs"] = serr.CurrentCIDRs
		}
		c.JSON(http.StatusConflict, body)
	default:
		internalErr(c, serr, "persist ssrf allowlist failed")
	}
}

// Put validates (shared pure validator), persists with compare-and-swap plus
// a same-transaction audit entry, and only then publishes the new policy to
// memory and broadcasts the invalidation.
func (h *SsrfAllowlistHandler) Put(c *gin.Context) {
	if !IsAdmin(c) {
		errorResp(c, http.StatusForbidden, ErrInsufficientPermissions, "platform admin role required to change the SSRF allowlist")
		return
	}
	if !h.requireRedisBackedReauth(c) {
		return
	}
	if !h.requireReauth(c) {
		return
	}
	var in ssrfAllowlistPutInput
	if err := c.ShouldBindJSON(&in); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if in.CIDRs == nil {
		errorResp(c, http.StatusBadRequest, ErrSsrfAllowlistInvalid, "cidrs is required; send an explicit empty array to enforce the strict policy, or use DELETE to remove the page override")
		return
	}
	res, serr := h.svc.Apply(c.Request.Context(), *in.CIDRs, in.ExpectedVersion, ssrfAllowlistActor(c))
	if serr != nil {
		h.writeError(c, serr)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

type ssrfAllowlistDeleteInput struct {
	ExpectedVersion int64 `json:"expected_version"`
}

// Delete tombstones the page override (env > YAML > strict fallback; the
// row and its monotonic version are kept — anti-ABA). Absent override with
// expected_version 0 is an idempotent success. expected_version may arrive
// as a JSON body or a ?expected_version= query parameter.
func (h *SsrfAllowlistHandler) Delete(c *gin.Context) {
	if !IsAdmin(c) {
		errorResp(c, http.StatusForbidden, ErrInsufficientPermissions, "platform admin role required to change the SSRF allowlist")
		return
	}
	if !h.requireRedisBackedReauth(c) {
		return
	}
	if !h.requireReauth(c) {
		return
	}
	var in ssrfAllowlistDeleteInput
	_ = c.ShouldBindJSON(&in)
	if q := c.Query("expected_version"); q != "" {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil {
			in.ExpectedVersion = v
		} else {
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "expected_version must be an integer")
			return
		}
	}
	res, serr := h.svc.Reset(c.Request.Context(), in.ExpectedVersion, ssrfAllowlistActor(c))
	if serr != nil {
		h.writeError(c, serr)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}
