package guardrail

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// 模型审核并入提示词注入规则后，一条规则可以同时带两个判据：
//   - 只开模型判据（rule_detection.enabled=false）时，模型照常执行；
//   - on_rule_hit=skip 且规则判据已命中时，不再调用模型；
//   - 方向为 response 时不调用模型。
func TestCompositeRuleModelReview(t *testing.T) {
	capture := func(t *testing.T) *bytes.Buffer {
		t.Helper()
		var audit bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&audit, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		return &audit
	}
	composite := func(onHit string, detection bool) datatypes.JSON {
		return datatypes.JSON(`{"rule_detection":{"enabled":` + boolStr(detection) + `},"model_review":{"enabled":true,"service_id":999,"incomplete":"block_alert","on_rule_hit":"` + onHit + `"}}`)
	}

	t.Run("规则判据关闭时模型照常执行", func(t *testing.T) {
		audit := capture(t)
		s := NewGuardrailService(setupSeedDB(t), nil)
		org := int64(7)
		legacy, finish := s.prepareReviews(t.Context(), &CheckRequest{OrgID: org, Content: "正文", Direction: DirectionRequest}, []GuardrailRule{
			{ID: 5, Type: PromptInjectionRule, OrgID: &org, Action: "log", Config: composite("skip", false)},
		})
		require.Empty(t, legacy, "复合规则不应再作为传统引擎执行")
		_, err := finish(&CheckResponse{}, nil)
		require.NoError(t, err)
		require.Contains(t, audit.String(), `"service_id":999`)
		require.Contains(t, audit.String(), `"on_rule_hit":"skip"`)
	})

	t.Run("规则判据命中且 skip 时不再调用模型", func(t *testing.T) {
		audit := capture(t)
		s := NewGuardrailService(setupSeedDB(t), nil)
		org := int64(7)
		_, finish := s.prepareReviews(t.Context(), &CheckRequest{OrgID: org, Content: "正文", Direction: DirectionRequest}, []GuardrailRule{
			{ID: 6, Type: PromptInjectionRule, OrgID: &org, Action: "block", Config: composite("skip", true)},
		})
		// 模拟规则判据先命中：引擎侧会把命中写进 Matched
		pre := &CheckResponse{Matched: []MatchedRule{{RuleID: 6, Action: "block", EngineType: PromptInjectionRule}}, Blocked: true}
		out, err := finish(pre, nil)
		require.NoError(t, err)
		require.True(t, out.Blocked)
		require.NotContains(t, audit.String(), "Prompt 模型审核", "skip 时不应再发起模型调用")
	})

	t.Run("规则判据命中但 run 时仍调用模型", func(t *testing.T) {
		audit := capture(t)
		s := NewGuardrailService(setupSeedDB(t), nil)
		org := int64(7)
		_, finish := s.prepareReviews(t.Context(), &CheckRequest{OrgID: org, Content: "正文", Direction: DirectionRequest}, []GuardrailRule{
			{ID: 7, Type: PromptInjectionRule, OrgID: &org, Action: "block", Config: composite("run", true)},
		})
		pre := &CheckResponse{Matched: []MatchedRule{{RuleID: 7, Action: "block", EngineType: PromptInjectionRule}}, Blocked: true}
		_, err := finish(pre, nil)
		require.NoError(t, err)
		require.Contains(t, audit.String(), `"on_rule_hit":"run"`)
	})

	t.Run("响应方向不调用模型", func(t *testing.T) {
		audit := capture(t)
		s := NewGuardrailService(setupSeedDB(t), nil)
		org := int64(7)
		_, finish := s.prepareReviews(t.Context(), &CheckRequest{OrgID: org, Content: "正文", Direction: DirectionResponse}, []GuardrailRule{
			{ID: 8, Type: PromptInjectionRule, OrgID: &org, Action: "log", Config: composite("run", true)},
		})
		_, err := finish(&CheckResponse{}, nil)
		require.NoError(t, err)
		require.False(t, strings.Contains(audit.String(), "Prompt 模型审核"))
	})
}

func boolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
