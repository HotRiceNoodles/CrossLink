package license

import (
	"testing"
	"time"
)

// U-01: commercial-tier schema objects (sso_providers, pat_tokens,
// prompt_templates, mock_fixtures, provider_guard_alert_rules,
// api_keys.price_multiplier/key_source) only exist in migrations/postgres.
// On MySQL/SQLite a licensed token must NOT unlock commercial features —
// every admin endpoint behind them would fail with "table not found".
// The Gate refuses to report a tier above Community unless the caller has
// explicitly marked the schema as supported (SetSchemaSupported(true),
// set by the commercial Init after checking the dialect).
func TestGateRequiresSchemaSupportForCommercialTiers(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).Unix()

	// Default gate (schema support not asserted): commercial token degrades.
	g := NewGate(&VerifiedToken{Tier: TierPro, ExpiresAt: future})
	if got := g.CurrentTier(); got != TierCommunity {
		t.Fatalf("tier without schema support = %q, want %q (commercial tables absent on this dialect)", got, TierCommunity)
	}
	// Feature gates follow the degraded tier.
	if err := g.RequirePro(); err == nil {
		t.Fatal("RequirePro should fail while schema support is not asserted")
	}

	// Same gate once the dialect is confirmed to carry the schema.
	g.SetSchemaSupported(true)
	if got := g.CurrentTier(); got != TierPro {
		t.Fatalf("tier with schema support = %q, want %q", got, TierPro)
	}
	if err := g.RequirePro(); err != nil {
		t.Fatalf("RequirePro with schema support: %v", err)
	}
}

func TestGateCommunityTierUnaffectedBySchemaFlag(t *testing.T) {
	g := CommunityGate()
	if got := g.CurrentTier(); got != TierCommunity {
		t.Fatalf("community gate tier = %q, want %q", got, TierCommunity)
	}
	// Degrading later (heartbeat path) must also respect the schema flag.
	g.SetSchemaSupported(true)
	g.SetToken(&VerifiedToken{Tier: TierEnterprise, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if got := g.CurrentTier(); got != TierEnterprise {
		t.Fatalf("enterprise with schema support = %q, want %q", got, TierEnterprise)
	}
	g.SetSchemaSupported(false)
	if got := g.CurrentTier(); got != TierCommunity {
		t.Fatalf("enterprise after schema support withdrawn = %q, want %q", got, TierCommunity)
	}
}
