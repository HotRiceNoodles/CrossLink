package admin

import (
	"encoding/json"

	"github.com/crosslink/internal/guardrail"
)

// 只读返回模型独立于持久化规则，避免能力字段进入数据库或版本快照。
type guardrailRuleResponse struct {
	guardrail.GuardrailRule
	StreamCapability guardrailStreamCapability `json:"stream_capability"`
}

type guardrailStreamCapability struct {
	Strategy           string   `json:"strategy"`
	ReasonCodes        []string `json:"reason_codes"`
	MaskSupported      bool     `json:"mask_supported"`
	ConfigurationError string   `json:"configuration_error,omitempty"`
}

// 能力由规则类型与配置决定，不由 block/mask 动作决定。
// 此处不构造引擎、不执行 Check，读取管理页面不会调用外部服务或启动后台任务。
func describeGuardrailStreamCapability(rule guardrail.GuardrailRule) guardrailStreamCapability {
	capability := guardrailStreamCapability{
		Strategy: "response_complete", ReasonCodes: []string{"whole_response_required"}, MaskSupported: rule.Type != "content_length",
	}
	switch rule.Type {
	case "keyword_filter":
		var config struct {
			UseRegex bool `json:"use_regex"`
		}
		if json.Unmarshal(rule.Config, &config) == nil && !config.UseRegex {
			capability.Strategy = "safe_prefix"
			capability.ReasonCodes = []string{"literal_candidate"}
		}
	case "credential_detection":
		var config struct {
			CustomPatterns []json.RawMessage `json:"custom_patterns"`
		}
		if json.Unmarshal(rule.Config, &config) == nil && len(config.CustomPatterns) == 0 {
			capability.Strategy = "safe_prefix"
			capability.ReasonCodes = []string{"credential_candidate"}
		}
	case "pii_detection":
		capability.Strategy = "safe_prefix"
		capability.ReasonCodes = []string{"pii_candidate"}
	case "content_length":
		capability.Strategy = "safe_prefix"
		capability.ReasonCodes = []string{}
	case "behavior_analysis":
		capability.Strategy = "request_only"
		capability.ReasonCodes = []string{"request_only"}
	case "agent_fingerprint":
		// 正文指纹可能来自当前指纹库及默认回退；无法静态排除时保守等待完整响应。
		// header/频次不因此重复执行，请求方向的规则仍在下方标记为 request_only。
	}
	if rule.Direction == "request" {
		capability.Strategy = "request_only"
		capability.ReasonCodes = []string{"request_only"}
	}
	if guardrail.ValidateRuleAction(rule.Type, rule.Action) != nil {
		capability.ConfigurationError = "content_length_no_mask"
	}
	return capability
}
