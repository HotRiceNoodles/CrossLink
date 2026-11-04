package guardrail

import (
	"context"
	"reflect"
	"testing"

	"gorm.io/datatypes"
)

func TestStreamHoldReasonsOnlyStableCodes(t *testing.T) {
	stream := &ResponseStream{rules: []streamRule{
		{rule: GuardrailRule{Name: "不应放入响应头的规则名", Type: "keyword_filter", Action: "mask", Config: datatypes.JSON(`{"blocklist":["private-content"]}`)}},
		{rule: GuardrailRule{Type: "pii_detection", Action: "block"}},
		{rule: GuardrailRule{Type: "credential_detection", Action: "mask"}},
		{rule: GuardrailRule{Type: "keyword_filter", Action: "block"}},
		{rule: GuardrailRule{Type: "content_length", Action: "block"}},
		{rule: GuardrailRule{Type: "webhook", Action: "mask"}, conservative: true},
	}}
	want := []string{"credential_candidate", "literal_candidate", "pii_candidate", "tool_json_complete", "whole_response_required"}
	if got := stream.HoldReasons(); !reflect.DeepEqual(got, want) {
		t.Fatalf("暂留能力未去重排序或混入了配置内容: got=%q want=%q", got, want)
	}
	returned := stream.HoldReasons()
	returned[0] = "caller-mutation"
	if got := stream.HoldReasons(); !reflect.DeepEqual(got, want) {
		t.Fatalf("外部修改返回值影响了固定能力: %q", got)
	}
}

func TestStreamHoldReasonsHonorsObservationAndEngineOverrides(t *testing.T) {
	tests := []struct {
		name         string
		kind, action string
		conservative bool
		logOnly      bool
		want         []string
	}{
		{"普通仅记录", "keyword_filter", "log", false, false, []string{"tool_json_complete"}},
		{"全局观察降级拦截", "keyword_filter", "block", false, true, []string{"tool_json_complete"}},
		{"全局观察保留脱敏", "keyword_filter", "mask", false, true, []string{"literal_candidate", "tool_json_complete"}},
		{"完整检查仅记录", "webhook", "log", true, false, []string{"tool_json_complete"}},
		{"合规分类可覆盖外层动作", "content_compliance", "log", true, false, []string{"tool_json_complete", "whole_response_required"}},
		{"全局观察降级合规分类", "content_compliance", "log", true, true, []string{"tool_json_complete"}},
		{"完整检查脱敏", "webhook", "mask", true, false, []string{"tool_json_complete", "whole_response_required"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stream := &ResponseStream{logOnly: tc.logOnly, rules: []streamRule{{rule: GuardrailRule{Type: tc.kind, Action: tc.action}, conservative: tc.conservative}}}
			if got := stream.HoldReasons(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("有效动作的暂留原因不正确: got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestStreamHoldReasonsUsesPinnedApplicableRules(t *testing.T) {
	svc := incrementalService()
	foreignOrg := int64(99)
	otherModel := "other-model"
	svc.rules = append(svc.rules,
		GuardrailRule{ID: 2, Enabled: true, Type: "credential_detection", Action: "mask", Direction: "response", OrgID: &foreignOrg, Config: datatypes.JSON(`{}`)},
		GuardrailRule{ID: 3, Enabled: true, Type: "keyword_filter", Action: "mask", Direction: "response", ModelFilter: &otherModel, Config: datatypes.JSON(`{"use_regex":true}`)},
		GuardrailRule{ID: 4, Enabled: true, Type: "pii_detection", Action: "mask", Direction: "request", Config: datatypes.JSON(`{}`)},
	)
	stream, err := svc.NewResponseStream(context.Background(), CheckRequest{Model: "test", OrgID: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	want := []string{"literal_candidate", "tool_json_complete"}
	if got := stream.HoldReasons(); !reflect.DeepEqual(got, want) {
		t.Fatalf("暂留原因混入其他组织、模型或请求方向规则: %q", got)
	}
	svc.rules = nil
	if got := stream.HoldReasons(); !reflect.DeepEqual(got, want) {
		t.Fatalf("规则变更影响了当前响应的固定能力: %q", got)
	}
}
