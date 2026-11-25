package guardrail

import "slices"

func (r GuardrailRule) matchesKey(id int64) bool {
	switch r.ScopeMode {
	case "", "all":
		return len(r.APIKeyIDs) == 0
	case "api_keys":
		// 无组织作用域规则（单租户 admin 所建，OrgID=nil）按 Key id 精确匹配
		return id > 0 && slices.Contains(r.APIKeyIDs, id)
	default:
		return false
	}
}
