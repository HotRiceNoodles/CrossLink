package middleware

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"
)

// One-time tokens authorizing the admin debug-replay path through Auth.
//
// The admin replay handler (internal/admin/debug.go) re-runs a logged request
// as its original key by loopback-calling this same process; keys are stored
// hashed, so the plaintext secret cannot be presented. Mint and redeem are
// therefore always same-process and no cross-instance state exists (or is
// possible) — a package-scoped store is the correct scope.
//
// Security properties (authentication-bypass remediation, reported 2026-10-28):
//   - 256-bit random, unguessable — replaces the old boundary "request source
//     looks loopback", which behind a same-host reverse proxy (the documented
//     nginx deployment) is true for EVERY external client when
//     server.trusted_proxies is unset.
//   - bound to a single key ID at mint time (the admin handler org-scopes it)
//   - single-use: consumed by the first redeem, never valid again
//   - 60s TTL: mint and the loopback request's Auth check are milliseconds
//     apart; the TTL bounds a token captured off the wire (loopback sniffing)

const (
	replayTokenTTL = 60 * time.Second
	replayTokenCap = 4096
)

type replayTokenEntry struct {
	keyID     int64
	expiresAt time.Time
}

type replayTokenStore struct {
	mu      sync.Mutex
	entries map[string]replayTokenEntry // sha256(token-hex) -> entry
}

var replayTokens = &replayTokenStore{entries: make(map[string]replayTokenEntry)}

// MintReplayToken issues a single-use token that authenticates one gateway
// request as keyID via Auth's replay branch. Returns "" (fail-closed: the
// caller must abort the replay) if the entropy source or the cap is exhausted.
func MintReplayToken(keyID int64) string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		slog.Error("replay token mint: crypto/rand unavailable", "error", err)
		return ""
	}
	token := hex.EncodeToString(raw)
	key := replayTokenIndex(token)

	replayTokens.mu.Lock()
	defer replayTokens.mu.Unlock()
	// Tokens are single-use, 60s-TTL, and minted only by authenticated admins,
	// so the map stays tiny. Sweep expired entries at the cap as a backstop
	// against unbounded growth (e.g. a future mint-without-redeem bug).
	if len(replayTokens.entries) >= replayTokenCap {
		now := time.Now()
		for k, e := range replayTokens.entries {
			if now.After(e.expiresAt) {
				delete(replayTokens.entries, k)
			}
		}
		if len(replayTokens.entries) >= replayTokenCap {
			slog.Error("replay token store at cap after sweep", "cap", replayTokenCap)
			return ""
		}
	}
	replayTokens.entries[key] = replayTokenEntry{keyID: keyID, expiresAt: time.Now().Add(replayTokenTTL)}
	return token
}

// redeemReplayToken consumes token and reports the key ID it was minted for.
// Missing, already-used, or expired tokens all return false — the Auth replay
// branch then falls through to normal (secret-based) authentication.
func redeemReplayToken(token string) (int64, bool) {
	if token == "" {
		return 0, false
	}
	key := replayTokenIndex(token)

	replayTokens.mu.Lock()
	defer replayTokens.mu.Unlock()
	e, ok := replayTokens.entries[key]
	if !ok {
		return 0, false
	}
	delete(replayTokens.entries, key) // single use, win or lose
	if time.Now().After(e.expiresAt) {
		return 0, false
	}
	return e.keyID, true
}

// replayTokenIndex maps a token to its store key. Storing the digest (not the
// token) means a process-memory dump of the map does not yield usable tokens.
func replayTokenIndex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
