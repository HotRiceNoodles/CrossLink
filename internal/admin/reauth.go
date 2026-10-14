package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ReauthManager implements SHORT-LIVED, SESSION-BOUND, SINGLE-USE
// re-authentication for sensitive admin operations (B5: the SSRF allowlist
// save/reset). Review requirements:
//
//  1. SESSION-BOUND: a token is bound to the ISSUING authenticated session,
//     identified by the irreversible fingerprint (SHA-256) of the current
//     access token. The raw token is never stored or logged — only the
//     hash. Another login session of the SAME user cannot use the token,
//     and after re-login (new access token ⇒ new fingerprint) old tokens
//     are dead. Issue and Verify both check the binding.
//  2. RATE-LIMITED WITHOUT A RACE: the per-subject attempt slot is
//     RESERVED ATOMICALLY BEFORE the (slow) bcrypt verification runs, so N
//     concurrent requests cannot all slip past the 5-attempt threshold.
//     Over-limit attempts receive the fixed ErrReauthRateLimited response.
//  3. MULTI-INSTANCE: when Redis is configured, tokens and failure counters
//     live in Redis with ATOMIC consume (GETDEL) and ATOMIC increment
//     (INCR+EXPIRE), shared by all replicas. Without Redis the manager
//     falls back to node-local state and reports Mode()=="local" — the
//     admin UI surfaces this so operators know re-auth tokens/limits are
//     per-node in that deployment shape.
//
// Credential handling: the password travels only in the reauth request
// body, is verified with the account's own bcrypt hash, and never appears
// in logs or error messages (fixed messages only).
const (
	reauthTokenTTL   = 2 * time.Minute
	reauthMaxFails   = 5
	reauthFailWindow = 10 * time.Minute
)

// Fixed messages — deliberately free of any credential-derived content.
var (
	ErrReauthInvalid     = errors.New("re-authentication failed: wrong password")
	ErrReauthRateLimited = errors.New("re-authentication rate limit exceeded; try again later")
	ErrReauthTokenBad    = errors.New("re-authentication required or token invalid/expired")
	ErrReauthUnavailable = errors.New("re-authentication temporarily unavailable; try again later")
)

// SessionFingerprint derives the stable, IRREVERSIBLE session identifier
// from the raw Authorization header value (e.g. "Bearer <jwt>"). The raw
// credential is never persisted or logged — only this SHA-256.
func SessionFingerprint(authorizationHeader string) string {
	sum := sha256.Sum256([]byte(authorizationHeader))
	return hex.EncodeToString(sum[:])
}

type reauthPayload struct {
	UserID      int64  `json:"u"`
	Fingerprint string `json:"f"`
}

type reauthEntry struct {
	userID      int64
	fingerprint string
	expires     time.Time
}

type reauthSubject struct {
	inflight int // verifications currently running (pre-claimed slots)
	fails    int // failed attempts in the window
	reset    time.Time
}

// ReauthManager is safe for concurrent use.
type ReauthManager struct {
	rdb *redis.Client // nil ⇒ node-local fallback
	now func() time.Time

	mu       sync.Mutex // guards the local fallback state below
	tokens   map[string]reauthEntry
	subjects map[int64]*reauthSubject
}

func NewReauthManager(rdb *redis.Client) *ReauthManager {
	return &ReauthManager{
		rdb:      rdb,
		now:      time.Now,
		tokens:   map[string]reauthEntry{},
		subjects: map[int64]*reauthSubject{},
	}
}

// Mode reports where re-auth state lives: "redis" (shared across replicas,
// atomic consume + rate limit) or "local" (node-only; surfaced in the UI).
func (m *ReauthManager) Mode() string {
	if m.rdb != nil {
		return "redis"
	}
	return "local"
}

func reauthFailKey(userID int64) string { return fmt.Sprintf("cl:reauth:fail:%d", userID) }
func reauthTokenKey(token string) string { // key by token hash — a Redis dump leaks nothing reusable
	sum := sha256.Sum256([]byte(token))
	return "cl:reauth:tok:" + hex.EncodeToString(sum[:])
}

// reauthClaimScript atomically reserves one attempt slot: INCR the subject's
// counter and arm the window expiry in the SAME script. Doing INCR and EXPIRE
// as two separate commands leaves a window where a crash between them strands
// the counter with NO TTL (permanent lockout); the script also repairs such
// legacy/no-TTL keys (TTL == -1) on the next call. Expiry mid-window simply
// starts the next claim at 1 with a fresh window.
var reauthClaimScript = redis.NewScript(`
local v = redis.call('INCR', KEYS[1])
if v == 1 or redis.call('TTL', KEYS[1]) == -1 then
	redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return v
`)

// reauthGetDelScript atomically reads and deletes the token key — the exact
// semantics of GETDEL, but via EVAL so it also works on Redis < 6.2 (GETDEL
// landed in 6.2; Redis 5.x is still common on Windows installs, and without
// this fallback every consume fails closed with ErrReauthUnavailable). A
// missing key makes Lua return false, which go-redis surfaces as redis.Nil —
// same contract as the native command.
var reauthGetDelScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v then
	redis.call('DEL', KEYS[1])
end
return v
`)

// Issue verifies the operator's password for THIS session (fingerprint) and
// mints a single-use token. The attempt slot is reserved BEFORE verification
// (atomic pre-claim) so concurrent requests cannot bypass the limit; a
// successful re-auth clears the failure window.
func (m *ReauthManager) Issue(userID int64, fingerprint, password string, verify func(password string) bool) (string, error) {
	if m.rdb != nil {
		return m.issueRedis(userID, fingerprint, password, verify)
	}
	return m.issueLocal(userID, fingerprint, password, verify)
}

func (m *ReauthManager) issueLocal(userID int64, fingerprint, password string, verify func(password string) bool) (string, error) {
	// Atomic pre-claim of an attempt slot (limits concurrency AND failures).
	m.mu.Lock()
	now := m.now()
	s := m.subjects[userID]
	if s != nil && now.After(s.reset) {
		delete(m.subjects, userID)
		s = nil
	}
	if s == nil {
		s = &reauthSubject{reset: now.Add(reauthFailWindow)}
		m.subjects[userID] = s
	}
	if s.inflight+s.fails >= reauthMaxFails {
		m.mu.Unlock()
		return "", ErrReauthRateLimited
	}
	s.inflight++
	m.mu.Unlock()

	ok := verify(password)

	m.mu.Lock()
	s = m.subjects[userID]
	if s != nil {
		s.inflight--
		if ok {
			s.fails = 0 // success clears the window
		} else {
			s.fails++
		}
	}
	m.mu.Unlock()
	if !ok {
		return "", ErrReauthInvalid
	}
	return m.mint(userID, fingerprint)
}

func (m *ReauthManager) issueRedis(userID int64, fingerprint, password string, verify func(password string) bool) (string, error) {
	ctx := context.Background()
	// Atomic pre-claim via a single Lua script (INCR + conditional EXPIRE):
	// no two-command gap, and a success (DEL) simply resets the window — a
	// concurrent failure INCRs from 1 again, which is the intended semantics.
	n, err := reauthClaimScript.Run(ctx, m.rdb, []string{reauthFailKey(userID)}, int(reauthFailWindow.Seconds())).Int64()
	if err != nil {
		return "", ErrReauthUnavailable // fail closed, fixed message
	}
	if n > reauthMaxFails {
		return "", ErrReauthRateLimited
	}
	if !verify(password) {
		return "", ErrReauthInvalid // counter stays — this attempt counts
	}
	// Success clears the window for the subject.
	if err := m.rdb.Del(ctx, reauthFailKey(userID)).Err(); err != nil {
		return "", ErrReauthUnavailable
	}
	return m.mint(userID, fingerprint)
}

func (m *ReauthManager) mint(userID int64, fingerprint string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("re-authentication token generation failed")
	}
	token := hex.EncodeToString(raw)
	if m.rdb != nil {
		payload, err := json.Marshal(reauthPayload{UserID: userID, Fingerprint: fingerprint})
		if err != nil {
			return "", ErrReauthUnavailable
		}
		if err := m.rdb.Set(context.Background(), reauthTokenKey(token), payload, reauthTokenTTL).Err(); err != nil {
			return "", ErrReauthUnavailable
		}
		return token, nil
	}
	m.mu.Lock()
	m.tokens[token] = reauthEntry{userID: userID, fingerprint: fingerprint, expires: m.now().Add(reauthTokenTTL)}
	for t, e := range m.tokens {
		if m.now().After(e.expires) {
			delete(m.tokens, t)
		}
	}
	m.mu.Unlock()
	return token, nil
}

// Verify consumes a token: it must exist, be unexpired, belong to the SAME
// user AND the SAME session fingerprint performing the sensitive operation.
// Single-use — the token is removed whether the check passes or fails
// (GETDEL in Redis mode: atomic across replicas, so concurrent consumers
// race safely and exactly one wins).
func (m *ReauthManager) Verify(userID int64, fingerprint, token string) error {
	if token == "" {
		return ErrReauthTokenBad
	}
	if m.rdb != nil {
		return m.verifyRedis(userID, fingerprint, token)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.tokens[token]
	delete(m.tokens, token)
	if !ok || m.now().After(entry.expires) || entry.userID != userID || entry.fingerprint != fingerprint {
		return ErrReauthTokenBad
	}
	return nil
}

func (m *ReauthManager) verifyRedis(userID int64, fingerprint, token string) error {
	ctx := context.Background()
	// Atomic single-use consume via the GETDEL-equivalent script (see
	// reauthGetDelScript for the < 6.2 compatibility rationale).
	raw, err := reauthGetDelScript.Run(ctx, m.rdb, []string{reauthTokenKey(token)}).Text()
	if err == redis.Nil {
		return ErrReauthTokenBad
	}
	if err != nil {
		return ErrReauthUnavailable // fail closed on Redis errors
	}
	var p reauthPayload
	if json.Unmarshal([]byte(raw), &p) != nil || p.UserID != userID || p.Fingerprint != fingerprint {
		return ErrReauthTokenBad // burned by the GETDEL above
	}
	return nil
}
