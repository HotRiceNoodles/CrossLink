package service

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
)

func anthropicReq(t *testing.T, raw string) *domain.AnthropicRequest {
	t.Helper()
	var req domain.AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &req
}

func TestAnalyzeAnthropic_BasicBuckets(t *testing.T) {
	req := anthropicReq(t, `{
		"model":"claude-3","max_tokens":1000,
		"system":"You are helpful.",
		"messages":[
			{"role":"user","content":"First question"},
			{"role":"assistant","content":"First answer"},
			{"role":"user","content":"Current question"}
		]
	}`)
	b := AnalyzeAnthropicBuckets(req)
	if b.SystemTokens == 0 || b.QuestionTokens == 0 {
		t.Fatalf("system/question must be > 0, got %+v", b)
	}
	if b.HistoryTokens == 0 {
		t.Fatalf("history must include first user+assistant turns")
	}
	if b.ToolTokens != 0 || b.ToolOutputTokens != 0 {
		t.Fatalf("no tools in request, got %+v", b)
	}
	if b.MessageCount != 3 {
		t.Errorf("MessageCount = %d, want 3", b.MessageCount)
	}
}

func TestAnalyzeAnthropic_LastUserToolResultOnly(t *testing.T) {
	// Agentic loop: last user message contains ONLY tool_result blocks.
	// Rule: tool_result -> tool_outputs bucket; question stays 0; no double count.
	req := anthropicReq(t, `{
		"model":"claude-3","max_tokens":1000,
		"messages":[
			{"role":"user","content":"List files"},
			{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"ls","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"file1\nfile2"}]}
		]
	}`)
	b := AnalyzeAnthropicBuckets(req)
	if b.QuestionTokens != 0 {
		t.Errorf("question must be 0 when last user msg has no text block, got %d", b.QuestionTokens)
	}
	if b.ToolOutputTokens == 0 {
		t.Errorf("tool_result text must land in tool_outputs bucket")
	}
}

func TestAnalyzeAnthropic_LastMessageAssistant(t *testing.T) {
	// Last message is assistant -> question = 0, content goes to history.
	req := anthropicReq(t, `{
		"model":"claude-3","max_tokens":1000,
		"messages":[
			{"role":"user","content":"Hello there kind assistant"},
			{"role":"assistant","content":"Hello"}
		]
	}`)
	b := AnalyzeAnthropicBuckets(req)
	if b.QuestionTokens != 0 || b.HistoryTokens == 0 {
		t.Errorf("assistant-last: question=0 history>0, got %+v", b)
	}
}

func TestAnalyzeAnthropic_NonTrailingUserIsHistory(t *testing.T) {
	// user, assistant, user, assistant: only a trailing user counts as question.
	req := anthropicReq(t, `{
		"model":"claude-3","max_tokens":1000,
		"messages":[
			{"role":"user","content":"First long question here folks"},
			{"role":"assistant","content":"First answer"},
			{"role":"user","content":"Second long question here folks"},
			{"role":"assistant","content":"Second answer"}
		]
	}`)
	b := AnalyzeAnthropicBuckets(req)
	if b.QuestionTokens != 0 {
		t.Errorf("non-trailing conversation must put all user turns in history, question got %d", b.QuestionTokens)
	}
	if b.HistoryTokens == 0 {
		t.Errorf("user turns must land in history")
	}
}

func TestAnalyzeAnthropic_ToolDefs(t *testing.T) {
	req := anthropicReq(t, `{
		"model":"claude-3","max_tokens":1000,
		"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"weather?"}]
	}`)
	b := AnalyzeAnthropicBuckets(req)
	if b.ToolTokens == 0 {
		t.Errorf("tool defs must count into tool_tokens")
	}
}

func openaiReq(t *testing.T, raw string) *domain.OpenAIRequest {
	t.Helper()
	var req domain.OpenAIRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &req
}

func TestAnalyzeOpenAI_BasicBuckets(t *testing.T) {
	req := openaiReq(t, `{
		"model":"gpt-4o","messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"First"},
			{"role":"assistant","content":"Answer"},
			{"role":"user","content":"Current question"}
		]
	}`)
	b := AnalyzeOpenAIBuckets(req)
	if b.SystemTokens == 0 || b.QuestionTokens == 0 || b.HistoryTokens == 0 {
		t.Fatalf("basic buckets wrong: %+v", b)
	}
}

func TestAnalyzeOpenAI_ToolFlow(t *testing.T) {
	req := openaiReq(t, `{
		"model":"gpt-4o","messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"72F sunny"},
			{"role":"user","content":"thanks, and tomorrow?"}
		]
	}`)
	b := AnalyzeOpenAIBuckets(req)
	if b.ToolOutputTokens == 0 {
		t.Errorf("role=tool content must land in tool_outputs")
	}
	if b.HistoryTokens == 0 {
		t.Errorf("earlier user turn + tool_calls must land in history")
	}
	if b.QuestionTokens == 0 {
		t.Errorf("last user message is the question")
	}
}

func TestAnalyzeOpenAI_LastMessageTool(t *testing.T) {
	// Last message is role=tool -> question = 0.
	req := openaiReq(t, `{
		"model":"gpt-4o","messages":[
			{"role":"user","content":"weather?"},
			{"role":"tool","tool_call_id":"c1","content":"72F sunny skies all day"}
		]
	}`)
	b := AnalyzeOpenAIBuckets(req)
	if b.QuestionTokens != 0 || b.ToolOutputTokens == 0 {
		t.Errorf("tool-last: question=0 tool_outputs>0, got %+v", b)
	}
}

func TestComputeAnalysisFlags(t *testing.T) {
	b := ContextBuckets{SystemTokens: 2000, HistoryTokens: 6100, QuestionTokens: 100, ToolTokens: 0, ToolOutputTokens: 100}
	window, maxTokens := 10000, 2000
	flags, bp := ComputeAnalysisFlags(b, window, maxTokens)
	if flags&FlagOverflowRisk == 0 {
		t.Errorf("8300+2000 > 10000 must set overflow bit")
	}
	if flags&FlagLongHistory == 0 {
		t.Errorf("history 6100 > 60%% of 10000 must set long-history bit")
	}
	if flags&FlagLongToolOutput != 0 {
		t.Errorf("tool_output 100 < 40%% must not set")
	}
	if bp != 8300 {
		t.Errorf("utilization = %d bp, want 8300", bp)
	}
}

func TestComputeAnalysisFlags_WindowUnknown(t *testing.T) {
	flags, bp := ComputeAnalysisFlags(ContextBuckets{QuestionTokens: 10}, 0, 100)
	if flags&FlagWindowUnknown == 0 {
		t.Errorf("window=0 must set unknown bit")
	}
	if bp != -1 {
		t.Errorf("window unknown -> bp = -1 sentinel, got %d", bp)
	}
}

func TestAnalyzeAnthropic_NoUserMessage(t *testing.T) {
	// Preflight-style request: system + tools only.
	req := anthropicReq(t, `{
		"model":"claude-3","max_tokens":1000,"system":"sys",
		"messages":[{"role":"assistant","content":"hi"}]
	}`)
	b := AnalyzeAnthropicBuckets(req)
	if b.QuestionTokens != 0 {
		t.Errorf("no user msg -> question 0")
	}
}

func TestDefaultContextWindow(t *testing.T) {
	cases := map[string]int{
		"claude-sonnet-4-5":     200000,
		"claude-3-5-haiku":      200000,
		"gpt-4o":                128000,
		"gpt-4o-mini":           128000,
		"o1":                    200000,
		"deepseek-chat":         64000,
		"qwen-max":              32000,
		"totally-unknown-model": 0, // 0 = unknown
		// Provider models are stored with vendor casing (e.g. GLM-4.7-Flash,
		// MiniMax-M2.7) — prefix match must be case-insensitive.
		"GLM-4.7-Flash":   128000,
		"GPT-4O-MINI":     128000,
		"MiniMax-M2.7":    200000,
		"DeepSeek-Chat":   64000,
	}
	for name, want := range cases {
		if got := DefaultContextWindow(name); got != want {
			t.Errorf("DefaultContextWindow(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestModelFamily_CaseInsensitive(t *testing.T) {
	cases := map[string]string{
		"GLM-4.7-Flash":  "glm",
		"DeepSeek-Chat":  "deepseek",
		"GPT-4O":         "gpt",
		"Claude-Sonnet":  "claude",
	}
	for name, want := range cases {
		if got := ModelFamily(name); got != want {
			t.Errorf("ModelFamily(%q) = %q, want %q", name, got, want)
		}
	}
}
