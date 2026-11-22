package guardapp

import (
	"encoding/json"

	"github.com/crosslink/internal/guardrail"
)

// 协议标识（payload.protocol）。
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
	ProtocolGeneric   = "generic"
	ProtocolResponses = "responses"
)

// IngestPayload 结构化检测 API 请求体（设计 DD-2）。Content 为纯 JSON 字符
// 串或结构化报文；Tenant 是客户声明的租户标识，与凭证映射的 org 做一致
// 性校验（不匹配 403），Metadata 原样透传进事件与检测上下文。Direction
// 缺省取端点语义（/detect 系=request，/traces/{id}/response=response）；
// Seq 是多次提交序号（幂等键组成，非流式通常缺省 0）。
type IngestPayload struct {
	TraceID   string          `json:"trace_id"`
	Protocol  string          `json:"protocol"`
	Direction string          `json:"direction,omitempty"`
	Seq       int64           `json:"seq,omitempty"`
	Model     string          `json:"model,omitempty"`
	Tenant    string          `json:"tenant,omitempty"`
	Content   json.RawMessage `json:"content"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// Normalized 是规范化层的输出：进入检测管线的检测文本及其元数据。
type Normalized struct {
	Input         *guardrail.DetectionInput
	Text          string
	Uninspectable bool // 结构化报文携带非文本媒体块（网关 strict coverage 同语义）
	Model         string
	Protocol      string
}

// protocolPath 把协议映射到共享抽取层的分发路径（guardrail.ExtractContent
// 按 path 后缀选择 extractor）。
func protocolPath(protocol string) string {
	switch protocol {
	case ProtocolOpenAI:
		return "/v1/chat/completions"
	case ProtocolAnthropic:
		return "/v1/messages"
	case ProtocolResponses:
		return "/v1/responses"
	}
	return ""
}

// NormalizeContent 把 payload.content 规范化为检测文本。两个入口（结构化
// API 与 M2 镜像端点）共用本函数，检测管线无感知接入方式差异：
//   - 纯 JSON 字符串 → 原文文本（任意协议）；
//   - openai/anthropic 且 content 为对象 → 视为该协议完整请求体，交给
//     guardrail.ExtractContent 抽取；content 为数组 → 包一层
//     {"messages": content} 再抽取；
//   - generic 且 content 为对象 → 直取其 "content" 字符串字段。
//
// ok=false 表示无可检文本（畸形报文/空内容），调用方据此拒绝或记事件。
func NormalizeContent(content json.RawMessage, protocol string) (text string, uninspectable bool, ok bool) {
	n, ok := NormalizeInput(content, protocol)
	return n.Text, n.Uninspectable, ok
}

// NormalizeInput 保留协议片段；纯正文不推测角色、来源或可信任务。
func NormalizeInput(content json.RawMessage, protocol string) (Normalized, bool) {
	plain := func(text string) (Normalized, bool) {
		return Normalized{Text: text, Protocol: protocol, Input: guardrail.PlainDetectionInput(text)}, true
	}
	raw := trimJSON(content)
	if len(raw) == 0 {
		return Normalized{}, false
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return Normalized{}, false
		}
		return plain(s)
	case '{':
		if path := protocolPath(protocol); path != "" {
			return extractBody(raw, path)
		}
		var obj struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(raw, &obj) != nil || obj.Content == "" {
			return Normalized{}, false
		}
		return plain(obj.Content)
	case '[':
		path := protocolPath(protocol)
		if path == "" {
			return Normalized{}, false
		}
		key := "messages"
		if protocol == ProtocolResponses {
			key = "input"
		}
		wrapped, err := json.Marshal(map[string]json.RawMessage{key: raw})
		if err != nil {
			return Normalized{}, false
		}
		return extractBody(wrapped, path)
	}
	return Normalized{}, false
}

func extractBody(body []byte, path string) (Normalized, bool) {
	ex := guardrail.ExtractContent(body, path)
	if ex == nil || ex.Text == "" {
		return Normalized{}, false
	}
	return Normalized{Text: ex.Text, Uninspectable: ex.Uninspectable, Input: ex.DetectionInput()}, true
}

// trimJSON 去掉 JSON 值外的空白。
func trimJSON(raw json.RawMessage) json.RawMessage {
	for len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t' || raw[0] == '\r' || raw[0] == '\n') {
		raw = raw[1:]
	}
	return raw
}

// MetadataMap 把 payload.metadata 解析为引擎上下文的键值对；非对象或为空
// 返回 nil。行为分析/指纹引擎按网关同语义读取 api_key_id 等键。
func MetadataMap(metadata json.RawMessage) map[string]string {
	raw := trimJSON(metadata)
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
