package license

import (
	"testing"

	"github.com/crosslink/internal/model"
)

// TestCommunityTierIncludesTemplateActions: prompt templates are a community-core
// feature (assembly engine + CRUD). Their actions MUST be in the community tier
// allowlist, else EffectiveActions filters them out and the admin frontend never
// receives template:list → the "Prompt Templates" menu is hidden in community.
func TestCommunityTierIncludesTemplateActions(t *testing.T) {
	for _, a := range []string{"template:list", "template:create", "template:update", "template:delete"} {
		if !TierActionSet[TierCommunity][a] {
			t.Errorf("community tier must allow %s (otherwise the templates menu is hidden)", a)
		}
	}
}

// TestScopedPATActionsRegistered: budget:read/health:read/pat:manage must be in
// all three registration tables (ValidActions, AdminExclusiveActions where
// applicable, and communityActions) — missing communityActions silently filters
// them out of EffectiveActions on Community tier (admin has permission but 403).
func TestScopedPATActionsRegistered(t *testing.T) {
	scopedPATActions := []string{"budget:read", "health:read", "pat:manage"}

	for _, a := range scopedPATActions {
		if !model.ValidActions[a] {
			t.Errorf("model.ValidActions must include %s", a)
		}
		if !communityActions[a] {
			t.Errorf("communityActions must include %s (otherwise Community tier 403s)", a)
		}
		if !TierActionSet[TierCommunity][a] {
			t.Errorf("community tier must allow %s", a)
		}
	}

	if !model.AdminExclusiveActions["pat:manage"] {
		t.Error("pat:manage must be admin-exclusive")
	}
	for _, a := range []string{"budget:read", "health:read"} {
		if model.AdminExclusiveActions[a] {
			t.Errorf("%s must NOT be admin-exclusive", a)
		}
	}
}
