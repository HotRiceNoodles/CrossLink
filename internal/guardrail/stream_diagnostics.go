package guardrail

import "sort"

// HoldReasons 返回当前响应固定规则可能产生的暂留原因，不代表逐 token 的等待状态。
// 返回值仅包含稳定能力原因码，不包含规则名、配置、命中内容或运行时错误文本。
func (r *ResponseStream) HoldReasons() []string {
	reasons := map[string]bool{"tool_json_complete": true}
	for i := range r.rules {
		sr := &r.rules[i]
		if sr.conservative {
			if r.ruleEnforces(sr) {
				reasons["whole_response_required"] = true
			}
			continue
		}
		if r.action(sr.rule) == "log" {
			continue
		}
		switch sr.rule.Type {
		case "keyword_filter":
			reasons["literal_candidate"] = true
		case "pii_detection":
			reasons["pii_candidate"] = true
		case "credential_detection":
			reasons["credential_candidate"] = true
		}
	}
	result := make([]string, 0, len(reasons))
	for reason := range reasons {
		result = append(result, reason)
	}
	sort.Strings(result)
	return result
}
