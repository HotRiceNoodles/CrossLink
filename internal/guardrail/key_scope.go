package guardrail

import "slices"

func (r GuardrailRule) matchesKey(id int64) bool {
	switch r.ScopeMode {
	case "", "all":
		return len(r.APIKeyIDs) == 0
	case "api_keys":
		return r.OrgID != nil && id > 0 && slices.Contains(r.APIKeyIDs, id)
	default:
		return false
	}
}
