package guardrail

import "testing"

// 提示词注入是对整段输入是否越权的判断，无法靠遮盖片段放行：只能是 log 或 block。
// 长度限制同理（遮盖不等于长度控制）。其它引擎仍允许 mask。
func TestValidateRuleAction(t *testing.T) {
	cases := []struct {
		kind    string
		action  string
		wantErr bool
	}{
		{"prompt_injection", "mask", true},
		{"prompt_injection", "block", false},
		{"prompt_injection", "log", false},
		{"content_length", "mask", true},
		{"content_length", "block", false},
		{"pii_detection", "mask", false},
		{"keyword_filter", "mask", false},
		{"model_review", "block", false},
	}
	for _, c := range cases {
		err := ValidateRuleAction(c.kind, c.action)
		if c.wantErr && err == nil {
			t.Errorf("%s/%s 应被拒绝，实际通过", c.kind, c.action)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s/%s 应被允许，实际拒绝：%v", c.kind, c.action, err)
		}
	}
}
