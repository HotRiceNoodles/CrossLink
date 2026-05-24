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

// currentTier returns the current license tier.
// NOTE: In Community mode, this always returns TierCommunity via CommunityGate(),
// which is hardcoded and cannot be tampered. The commercial edition validates
// the token signature before trusting the Tier field.
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
