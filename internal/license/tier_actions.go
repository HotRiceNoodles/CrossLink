package license

import "sort"

// communityActions defines the base set of actions available to the Community tier.
var communityActions = map[string]bool{
	"provider:list":     true,
	"provider:create":   true,
	"provider:update":   true,
	"provider:delete":   true,
	"provider:test":     true,
	"model:list":        true,
	"model:create":      true,
	"model:update":      true,
	"model:delete":      true,
	"key:list":          true,
	"key:create":        true,
	"key:update":        true,
	"key:delete":        true,
	"key:regenerate":    true,
	"usage:list":        true,
	"usage:stats":       true,
	"routing:stats":     true,
	"system:password":   true,
	"license:view":      true,
	"license:manage":    true,
	"mcp:list":          true,
	"mcp:view":          true,
	"mcp:create":        true,
	"mcp:update":        true,
	"mcp:delete":        true,
	"mcp:permission":    true,
	"mcp:logs":          true,
	"mcp:stats":         true,
	"error_rule:list":   true,
	"error_rule:create": true,
	"error_rule:update": true,
	"error_rule:delete": true,
}

// proExtraActions defines additional actions available to the Pro tier (on top of Community).
var proExtraActions = map[string]bool{
	"debug:list":                  true,
	"debug:clear":                 true,
	"key:rotate":                  true,
	"key:hashes":                  true,
	"usage:export":                true,
	"system:view":                 true,
	"system:update":               true,
	// Capability alias admin CRUD is Pro-gated end-to-end: the resolver, repo,
	// and admin handler all live in the commercial overlay, which is only built
	// into Pro/Enterprise. Runtime usage is additionally gated via the alias
	// resolver's RequirePro check.
	"capability:list":             true,
	"capability:create":           true,
	"capability:update":           true,
	"capability:delete":           true,
	"guardrail:list":              true,
	"guardrail:create":            true,
	"guardrail:update":            true,
	"guardrail:delete":            true,
	"guardrail:test":              true,
	"guardrail_alert:list":        true,
	"guardrail_alert:create":      true,
	"guardrail_alert:update":      true,
	"guardrail_alert:delete":      true,
	"guardrail_alert:logs":        true,
	"playground:use":              true,
	"secret:test":                 true,
	"secret:manage":               true,
	"agent_shield:view":           true,
	"agent_shield:manage":         true,
	"fingerprint:list":            true,
	"fingerprint:view":            true,
	"fingerprint:manage":          true,
	"datalens:view":               true,
	"datalens:create":             true,
	"datalens:update":             true,
	"datalens:delete":             true,
	"datalens:export":             true,
	"datalens:schedule":           true,
	"datalens:manage_aggregation": true,
}

// enterpriseExtraActions defines additional actions available to the Enterprise tier (on top of Pro).
var enterpriseExtraActions = map[string]bool{
	"team:list":           true,
	"team:create":         true,
	"team:update":         true,
	"team:delete":         true,
	"team:manage_members": true,
	"user:list":           true,
	"user:create":         true,
	"user:update":         true,
	"user:delete":         true,
	"audit:list":          true,
	"audit:export":        true,
	"budget:manage":       true,
	"insight:manage":      true,
	"org:list":            true,
	"org:create":          true,
	"org:update":          true,
	"org:delete":          true,
	"org:manage_members":  true,
	"org:view_billing":    true,
	"org:manage_billing":  true,
	"role:list":           true,
	"role:create":         true,
	"role:update":         true,
	"role:delete":         true,
}

// TierActionSet maps each tier name to its fully enumerated set of allowed actions.
var TierActionSet map[string]map[string]bool

func init() {
	// Community: just the base set.
	community := make(map[string]bool, len(communityActions))
	for k := range communityActions {
		community[k] = true
	}
	TierActionSet = map[string]map[string]bool{
		TierCommunity: community,
	}

	// Pro: Community + pro extras.
	pro := make(map[string]bool, len(communityActions)+len(proExtraActions))
	for k := range communityActions {
		pro[k] = true
	}
	for k := range proExtraActions {
		pro[k] = true
	}
	TierActionSet[TierPro] = pro

	// Enterprise: Pro's full set + enterprise extras.
	enterprise := make(map[string]bool, len(pro)+len(enterpriseExtraActions))
	for k := range pro {
		enterprise[k] = true
	}
	for k := range enterpriseExtraActions {
		enterprise[k] = true
	}
	TierActionSet[TierEnterprise] = enterprise
}

// TierAllowsAction checks whether the current tier permits the given action.
func TierAllowsAction(action string) bool {
	actions, ok := TierActionSet[G().CurrentTier()]
	if !ok {
		return false
	}
	return actions[action]
}

// EffectiveActions returns the intersection of dbActions and the current tier's
// allowed actions, sorted alphabetically.
func EffectiveActions(dbActions []string) []string {
	tierActions := TierActionSet[G().CurrentTier()]
	var result []string
	for _, a := range dbActions {
		if tierActions[a] {
			result = append(result, a)
		}
	}
	sort.Strings(result)
	return result
}

// TierAllowedActions returns all actions allowed for the current tier, sorted alphabetically.
func TierAllowedActions() []string {
	tierActions := TierActionSet[G().CurrentTier()]
	result := make([]string, 0, len(tierActions))
	for a := range tierActions {
		result = append(result, a)
	}
	sort.Strings(result)
	return result
}
