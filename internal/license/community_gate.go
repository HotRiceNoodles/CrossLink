package license

import (
	"fmt"
	"log/slog"
	"sync/atomic"
)

// Gate provides thread-safe tier checking. It implements app.GateInterface.
type Gate struct {
	token       atomic.Pointer[VerifiedToken]
	stopCh      chan struct{}
	fingerprint string
	// schemaOK gates commercial tiers: their tables/columns exist only in
	// migrations/postgres (U-01). False until the caller confirms the active
	// dialect carries the commercial schema via SetSchemaSupported.
	schemaOK     atomic.Bool
	schemaWarned atomic.Bool
}

// global is the package-level gate instance, set by Init or CommunityInit.
var global *Gate

func NewGate(token *VerifiedToken) *Gate {
	g := &Gate{stopCh: make(chan struct{})}
	g.token.Store(token)
	return g
}

func (g *Gate) SetToken(t *VerifiedToken) {
	g.token.Store(t)
}

// SetSchemaSupported marks whether the database dialect carries the
// commercial schema objects (Postgres/Kingbase only — their migrations live
// exclusively in migrations/postgres). Commercial tiers are refused while
// false; community features are never affected.
func (g *Gate) SetSchemaSupported(ok bool) {
	g.schemaOK.Store(ok)
}

// currentTier returns the current license tier.
// NOTE: In Community mode, this always returns TierCommunity via CommunityGate(),
// which is hardcoded and cannot be tampered. The commercial edition does NOT
// verify the token signature locally (U-37, documented): it binds trust to
// expiry + node fingerprint enforcement on every load path (DB, offline file,
// heartbeat cache). Signature verification with a distributed public key is
// the tracked hardening item — do NOT claim it exists.
func (g *Gate) currentTier() string {
	t := g.token.Load()
	if t == nil {
		return TierCommunity
	}
	tier := t.Tier
	if tier != TierCommunity && tier != TierPro && tier != TierEnterprise {
		slog.Warn("invalid license tier detected, falling back to community", "tier", tier)
		return TierCommunity
	}
	// U-01: commercial-tier schema objects (sso_providers, pat_tokens,
	// prompt_templates, mock_fixtures, provider_guard_alert_rules,
	// api_keys.price_multiplier/key_source) have migrations only under
	// migrations/postgres. On other dialects the tables do not exist, so a
	// licensed token must degrade to Community instead of exposing endpoints
	// that fail with "table not found".
	if tier != TierCommunity && !g.schemaOK.Load() {
		if g.schemaWarned.CompareAndSwap(false, true) {
			slog.Error("license tier requires the Postgres/Kingbase schema; commercial tables are absent on this dialect — running as Community until the database is migrated", "tier", tier)
		}
		return TierCommunity
	}
	return tier
}

func (g *Gate) RequirePro() error {
	if tier := g.currentTier(); tier == TierPro || tier == TierEnterprise {
		return nil
	}
	return fmt.Errorf("this feature requires a Pro or Enterprise license (current: %s)", g.currentTier())
}

func (g *Gate) RequireEnterprise() error {
	if tier := g.currentTier(); tier == TierEnterprise {
		return nil
	}
	return fmt.Errorf("this feature requires an Enterprise license (current: %s)", g.currentTier())
}

func (g *Gate) CurrentTier() string {
	return g.currentTier()
}

// G returns the global Gate instance for handler-level tier checks.
func G() *Gate {
	if global == nil {
		return CommunityGate()
	}
	return global
}

// CommunityGate returns a Gate always in Community mode.
func CommunityGate() *Gate {
	return NewGate(&VerifiedToken{Tier: TierCommunity})
}

// TokenSnapshot returns a copy of the current token fields for API responses.
func (g *Gate) TokenSnapshot() *VerifiedToken {
	t := g.token.Load()
	if t == nil {
		return nil
	}
	return t
}

func (g *Gate) IsValid() bool {
	t := g.token.Load()
	if t == nil {
		return false
	}
	return t.isValid()
}

func (g *Gate) RawToken() string {
	t := g.token.Load()
	if t == nil {
		return ""
	}
	return t.RawToken
}

func (g *Gate) Fingerprint() string {
	return g.fingerprint
}

func (g *Gate) Stop() {
	select {
	case <-g.stopCh:
	default:
		close(g.stopCh)
	}
}
