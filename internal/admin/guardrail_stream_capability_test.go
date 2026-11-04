package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/crosslink/internal/guardrail"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestGuardrailStreamCapabilitiesDescribeConfigurationAndNotAction(t *testing.T) {
	tests := []struct {
		kind, config, strategy, reason string
		mask                           bool
	}{
		{"keyword_filter", `{"blocklist":["secret"]}`, "safe_prefix", "literal_candidate", true},
		{"keyword_filter", `{"blocklist":["a.*b"],"use_regex":true}`, "response_complete", "whole_response_required", true},
		{"pii_detection", `{"types":["phone"]}`, "safe_prefix", "pii_candidate", true},
		{"credential_detection", `{}`, "safe_prefix", "credential_candidate", true},
		{"credential_detection", `{"custom_patterns":[{"name":"local","pattern":"private.*value"}]}`, "response_complete", "whole_response_required", true},
		{"content_length", `{"max_output_chars":10}`, "safe_prefix", "", false},
		{"behavior_analysis", `{}`, "request_only", "request_only", true},
		{"agent_fingerprint", `{"mode":"block"}`, "response_complete", "whole_response_required", true},
		{"openai_moderation", `{"api_key":"do-not-echo-this-secret"}`, "response_complete", "whole_response_required", true},
		{"webhook", `{"url":"https://example.com"}`, "response_complete", "whole_response_required", true},
		{"content_compliance", `{"semantic":{"api_key":"do-not-echo-this-secret"}}`, "response_complete", "whole_response_required", true},
		{"future_engine", `{}`, "response_complete", "whole_response_required", true},
	}
	for _, tc := range tests {
		t.Run(tc.kind+tc.config, func(t *testing.T) {
			for _, action := range []string{"block", "mask"} {
				rule := guardrail.GuardrailRule{ID: 1, Name: "规则名", Type: tc.kind, Direction: "response", Config: datatypes.JSON(tc.config), Action: action, Version: 3}
				raw, err := json.Marshal(sanitizeRule(rule))
				require.NoError(t, err)
				var value map[string]any
				require.NoError(t, json.Unmarshal(raw, &value))
				require.Contains(t, value, "stream_capability", "规则返回需包含只读能力")
				capability := value["stream_capability"].(map[string]any)
				require.Equal(t, tc.strategy, capability["strategy"])
				require.Equal(t, tc.mask, capability["mask_supported"])
				if tc.reason != "" {
					require.Contains(t, capability["reason_codes"], tc.reason)
				} else {
					require.Empty(t, capability["reason_codes"])
				}
				require.NotContains(t, string(raw), "do-not-echo-this-secret")
				require.EqualValues(t, 1, value["id"])
				require.EqualValues(t, 3, value["version"])
				if tc.kind == "content_length" && action == "mask" {
					require.Equal(t, "content_length_no_mask", capability["configuration_error"])
				}
			}
		})
	}
}

func TestGuardrailRequestDirectionDoesNotHoldResponse(t *testing.T) {
	raw, err := json.Marshal(sanitizeRule(guardrail.GuardrailRule{Type: "keyword_filter", Direction: "request", Config: datatypes.JSON(`{}`), Action: "mask"}))
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(raw, &value))
	require.Contains(t, value, "stream_capability")
	require.Equal(t, "request_only", value["stream_capability"].(map[string]any)["strategy"])
}

func TestGuardrailCreateRejectsLengthMaskWithoutHistory(t *testing.T) {
	env := newM3AEnv(t, false, 0)
	w := env.req(t, http.MethodPost, "/guardrails", map[string]any{
		"name": "非法长度脱敏", "type": "content_length", "direction": "response", "config": map[string]any{"max_output_chars": 10}, "action": "mask",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var rules, versions int64
	require.NoError(t, env.db.Model(&guardrail.GuardrailRule{}).Count(&rules).Error)
	require.NoError(t, env.db.Model(&guardrail.GuardrailRuleVersion{}).Count(&versions).Error)
	require.Zero(t, rules)
	require.Zero(t, versions)
}

func TestGuardrailUpdateValidatesMergedTypeAndAction(t *testing.T) {
	for _, patch := range []map[string]any{
		{"action": "mask"},
		{"type": "content_length", "action": "mask", "config": map[string]any{"max_output_chars": 10}},
	} {
		t.Run(fmt.Sprint(patch), func(t *testing.T) {
			env := newM3AEnv(t, false, 0)
			rule := guardrail.GuardrailRule{Name: "长度限制", Type: "content_length", Direction: "response", Config: datatypes.JSON(`{"max_output_chars":10}`), Action: "block", Version: 1}
			require.NoError(t, env.db.Create(&rule).Error)
			w := env.req(t, http.MethodPut, fmt.Sprintf("/guardrails/%d", rule.ID), patch)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var saved guardrail.GuardrailRule
			require.NoError(t, env.db.First(&saved, rule.ID).Error)
			require.Equal(t, "block", saved.Action)
			require.EqualValues(t, 1, saved.Version)
			require.Empty(t, env.history(t, rule.ID))
		})
	}
}

func TestGuardrailUpdateRejectsTypeOnlyConversionToLengthMask(t *testing.T) {
	env := newM3AEnv(t, false, 0)
	rule := guardrail.GuardrailRule{Name: "原关键词脱敏", Type: "keyword_filter", Direction: "response", Config: datatypes.JSON(`{"blocklist":["secret"]}`), Action: "mask", Version: 1}
	require.NoError(t, env.db.Create(&rule).Error)
	w := env.req(t, http.MethodPut, fmt.Sprintf("/guardrails/%d", rule.ID), map[string]any{"type": "content_length"})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var saved guardrail.GuardrailRule
	require.NoError(t, env.db.First(&saved, rule.ID).Error)
	require.Equal(t, "keyword_filter", saved.Type)
	require.EqualValues(t, 1, saved.Version)
}

func TestGuardrailLegacyInvalidLengthCanOnlyDisableOrRepair(t *testing.T) {
	env := newM3AEnv(t, false, 0)
	rule := guardrail.GuardrailRule{Name: "历史长度脱敏", Type: "content_length", Direction: "response", Enabled: true, Config: datatypes.JSON(`{}`), Action: "mask", Version: 1}
	require.NoError(t, env.db.Create(&rule).Error)
	path := fmt.Sprintf("/guardrails/%d", rule.ID)
	for _, patch := range []map[string]any{{"name": "无效仍保留"}, {"enabled": false, "name": "混合修改"}, {"enabled": true}} {
		w := env.req(t, http.MethodPut, path, patch)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
	w := env.req(t, http.MethodPut, path, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var disabled guardrail.GuardrailRule
	require.NoError(t, env.db.First(&disabled, rule.ID).Error)
	require.False(t, disabled.Enabled)
	require.Equal(t, "mask", disabled.Action, "停用不能悄悄转换动作")
	w = env.req(t, http.MethodPut, path, map[string]any{"action": "log", "enabled": true})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var repaired guardrail.GuardrailRule
	require.NoError(t, env.db.First(&repaired, rule.ID).Error)
	require.Equal(t, "log", repaired.Action)
	require.True(t, repaired.Enabled)
}

func TestGuardrailRollbackRejectsInvalidHistoricalLengthMask(t *testing.T) {
	env := newM3AEnv(t, false, 0)
	rule := guardrail.GuardrailRule{Name: "长度限制", Type: "content_length", Direction: "response", Enabled: true, Config: datatypes.JSON(`{}`), Action: "block", Version: 2}
	require.NoError(t, env.db.Create(&rule).Error)
	history := snapshotVersion(rule, 1, "update", "tester")
	history.Action = "mask"
	history.Enabled = false
	require.NoError(t, env.db.Create(&history).Error)
	w := env.req(t, http.MethodPost, fmt.Sprintf("/guardrails/%d/rollback", rule.ID), map[string]any{"to_version": 1})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var saved guardrail.GuardrailRule
	require.NoError(t, env.db.First(&saved, rule.ID).Error)
	require.Equal(t, "block", saved.Action)
	require.EqualValues(t, 2, saved.Version)
	require.Len(t, env.history(t, rule.ID), 1)
}

func TestGuardrailCapabilityReadOnlyAndTenantScoped(t *testing.T) {
	env := newM3AEnv(t, false, 7)
	handler := NewGuardrailHandler(env.db, env.svcA, nil)
	env.router.GET("/guardrails", handler.List)
	w := env.req(t, http.MethodPost, "/guardrails", map[string]any{
		"name": "只读能力", "type": "keyword_filter", "direction": "response", "action": "mask", "config": map[string]any{"blocklist": []string{"secret"}, "token": "private-config-token"},
		"stream_capability": map[string]any{"strategy": "request_only", "mask_supported": false, "reason_codes": []string{"fake"}},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "private-config-token")
	require.NotContains(t, w.Body.String(), "fake")
	otherOrg := int64(9)
	require.NoError(t, env.db.Create(&guardrail.GuardrailRule{Name: "其他组织的规则", OrgID: &otherOrg, Type: "content_length", Direction: "response", Config: datatypes.JSON(`{}`), Action: "mask"}).Error)
	w = env.req(t, http.MethodGet, "/guardrails", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var list struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list.Data, 1)
	require.Contains(t, list.Data[0], "stream_capability")
	require.Equal(t, "safe_prefix", list.Data[0]["stream_capability"].(map[string]any)["strategy"])
	require.False(t, env.db.Migrator().HasColumn(&guardrail.GuardrailRule{}, "stream_capability"))
}
