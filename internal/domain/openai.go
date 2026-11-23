package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

type OpenAIRequest struct {
	Model               string          `json:"model"`
	Messages            []OpenAIMessage `json:"messages"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	N                   *int            `json:"n,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                any             `json:"stop,omitempty"`
	User                string          `json:"user,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Tools               []OpenAITool    `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Thinking            json.RawMessage `json:"thinking,omitempty"`
	// InboundSamplingMeta carries inbound Anthropic sampling state that the
	// internal representation cannot express (top_k) to the outbound adapter,
	// which enforces target-version rules at its serialization checkpoint
	// (M0). json:"-" keeps it out of every wire encoding — never sent
	// upstream, to clients, or accepted from client JSON.
	InboundSamplingMeta *InboundSamplingMeta `json:"-"`
	// InboundCacheMeta is the prompt-cache carry (system/tools half): original
	// Anthropic bytes that carry cache_control markers no structured rebuild
	// can preserve. Filled by AnthropicToOpenAI, consumed by
	// OpenAIToAnthropicRequest for same-protocol upstreams.
	InboundCacheMeta *InboundCacheMeta `json:"-"`

	// Generation params below are OPENAI-WIRE NATIVE: the openai_compatible
	// adapter marshals the struct straight to the upstream body, so these
	// fields pass through openai→openai verbatim. The anthropic reverse
	// translation simply does not map them (no protocol equivalent — dropped
	// cross-protocol, like n). Deliberately NOT included: service_tier
	// (priority processing = billing-coupled, stays observe-dropped at
	// admission until its pricing gate exists).
	ResponseFormat    json.RawMessage `json:"response_format,omitempty"` // structured outputs (json_object/json_schema), verbatim
	FrequencyPenalty  *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64        `json:"presence_penalty,omitempty"`
	Seed              *int            `json:"seed,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Logprobs          *bool           `json:"logprobs,omitempty"`
	TopLogprobs       *int            `json:"top_logprobs,omitempty"`
	LogitBias         json.RawMessage `json:"logit_bias,omitempty"` // token-id → bias map, verbatim
	Store             *bool           `json:"store,omitempty"`
	// Extensions is the M3 admission envelope: protocol-level parameters and
	// headers the gateway has ADMITTED (identified + validated) at the inbound
	// handler, carried to the outbound adapter checkpoint. Handlers are the
	// only writers; json:"-" blocks client injection. See
	// docs/design/protocol-envelope-and-admission.md §1.
	Extensions *ProtocolExtensions `json:"-"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// InboundSamplingMeta is the minimal M0 sampling carry: explicit presence and
// value of parameters the OpenAI-shaped internal request drops during
// AnthropicToOpenAI translation (top_k has no field there). Temperature/TopP
// already round-trip as first-class pointer fields and need no carry.
type InboundSamplingMeta struct {
	TopK *int `json:"-"`
}

// InboundCacheMeta is the request-level prompt-cache carry (the system/tools
// half; per-message markers ride on OpenAIMessage.CacheMarker). AnthropicToOpenAI
// captures the ORIGINAL system block-array and tools bytes — cache_control
// markers live inside them and no structured rebuild can preserve them — and
// OpenAIToAnthropicRequest re-emits them verbatim for same-protocol upstreams.
type InboundCacheMeta struct {
	// SystemRaw: the original system field when it was a block array (a plain
	// string system cannot carry markers and needs no carry).
	SystemRaw json.RawMessage
	// ToolsRaw: the original tools array bytes (per-tool cache_control).
	ToolsRaw json.RawMessage
}

// ProtocolExtensions is the M3 internal envelope (design §1): admitted
// protocol params and headers, written exclusively by the inbound handler
// after identification and validation, consumed at the outbound adapter
// checkpoint. Values are verbatim RawMessage — the envelope preserves value
// semantics of ADMITTED fields only; it is not a general passthrough.
type ProtocolExtensions struct {
	// SourceProtocol is the inbound protocol ("openai" | "anthropic"); it
	// decides which recovery rules apply at the outbound checkpoint.
	SourceProtocol string
	// Params holds admitted namespaced parameters (key convention:
	// "<namespace>.<param>", e.g. "sampling.top_k" once M0's carry merges in).
	Params map[string]json.RawMessage
	// Headers holds admitted protocol headers (first batch: none — the beta
	// token whitelist ships empty until tokens are verified per M2's dual
	// track).
	Headers map[string]string
}

// Clone returns a deep copy for the GatewayService attempt boundary — two
// candidates must never observe shared mutable state (design §1.3).
// RawMessage values are treated as immutable; only the maps are copied.
func (e *ProtocolExtensions) Clone() *ProtocolExtensions {
	if e == nil {
		return nil
	}
	cp := &ProtocolExtensions{SourceProtocol: e.SourceProtocol}
	if e.Params != nil {
		cp.Params = make(map[string]json.RawMessage, len(e.Params))
		for k, v := range e.Params {
			cp.Params[k] = append(json.RawMessage(nil), v...)
		}
	}
	if e.Headers != nil {
		cp.Headers = make(map[string]string, len(e.Headers))
		for k, v := range e.Headers {
			cp.Headers[k] = v
		}
	}
	return cp
}

type OpenAIMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	// CacheMarker carries the Anthropic prompt-cache marker of the inbound
	// content block(s) this message was translated FROM (json:"-" keeps it
	// off the OpenAI wire — the protocol has no such field). The reverse
	// translation re-attaches it onto the last rebuilt block. See
	// InboundCacheMeta for the system/tools level of the same carry.
	CacheMarker json.RawMessage `json:"-"`
	// ThinkingBlocks carries assistant thinking/redacted_thinking blocks
	// (values: thinking text + signature, redacted data) translated FROM an
	// anthropic inbound message (类二 M-A). The reverse translation prepends
	// them verbatim onto the rebuilt assistant content; openai-inbound never
	// sets them.
	ThinkingBlocks []ContentBlock `json:"-"`
}

// ContentText extracts a text string from Content, which may be nil, a string,
// or an array of content parts like [{"type":"text","text":"hello"}].
func ContentText(v any) string {
	if v == nil {
		return ""
	}
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				if m["type"] == "text" {
					if text, ok := m["text"].(string); ok && text != "" {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", v)
	}
}

type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAITool struct {
	Type     string            `json:"type"`
	Function OpenAIFunctionDef `json:"function"`
}

type OpenAIFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type OpenAIResponse struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	Choices           []OpenAIChoice `json:"choices"`
	Usage             OpenAIUsage    `json:"usage"`
	SystemFingerprint string         `json:"system_fingerprint,omitempty"`
	ServiceTier       string         `json:"service_tier,omitempty"`
	// ThinkingBlocks: thinking/redacted_thinking blocks captured from an
	// anthropic upstream response (values incl. signature/data — 类二 M-B),
	// re-emitted verbatim to anthropic-inbound clients by OpenAIToAnthropic.
	// json:"-" keeps them off the OpenAI wire.
	ThinkingBlocks []ContentBlock `json:"-"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
	// Logprobs rides verbatim when the client asked for it (request-side
	// Logprobs/TopLogprobs): passthrough keeps the pair functional
	// openai→openai; anthropic upstreams never populate it.
	Logprobs json.RawMessage `json:"logprobs,omitempty"`
}

type OpenAIUsage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	// CacheCreationTokens carries Anthropic cache_creation_input_tokens through
	// the internal pipeline (billing); never serialized to clients (json:"-").
	CacheCreationTokens int `json:"-"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// OpenAIChunk is a single SSE chunk from the streaming response.
type OpenAIChunk struct {
	ID                string              `json:"id"`
	Object            string              `json:"object"`
	Created           int64               `json:"created"`
	Model             string              `json:"model"`
	Choices           []OpenAIChunkChoice `json:"choices"`
	Usage             *OpenAIChunkUsage   `json:"usage,omitempty"`
	SystemFingerprint string              `json:"system_fingerprint,omitempty"`
}

type OpenAIChunkChoice struct {
	Index        int              `json:"index"`
	Delta        OpenAIChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

type OpenAIChunkDelta struct {
	Role             string                `json:"role,omitempty"`
	Content          string                `json:"content,omitempty"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIChunkToolCall `json:"tool_calls,omitempty"`
	// 类二 M-C stream carries (json:"-" — OpenAI wire has no such concepts):
	// SignatureDelta rides an anthropic signature_delta event through the
	// double translation so the client stream keeps the thinking signature;
	// RedactedThinking carries a redacted_thinking content_block_start's raw
	// content_block object to re-emit verbatim.
	SignatureDelta   string          `json:"-"`
	RedactedThinking json.RawMessage `json:"-"`
}

type OpenAIChunkToolCall struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id,omitempty"`
	Type     string                  `json:"type,omitempty"`
	Function OpenAIChunkFunctionCall `json:"function"`
}

type OpenAIChunkFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type OpenAIChunkUsage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	// CacheCreationTokens carries Anthropic cache_creation_input_tokens through
	// the internal pipeline (billing); never serialized to clients (json:"-").
	CacheCreationTokens int `json:"-"`
}

// SSEChunk wraps a parsed OpenAIChunk or a [DONE] signal.
type SSEChunk struct {
	Chunk *OpenAIChunk
	Done  bool
}
