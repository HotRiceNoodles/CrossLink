package license

import "testing"

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
