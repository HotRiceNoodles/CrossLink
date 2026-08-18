package handler

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
)

func intPtrBridge(v int) *int { return &v }

func TestBuildContextAnalysisResult_Anthropic(t *testing.T) {
	var req domain.AnthropicRequest
	json.Unmarshal([]byte(`{"model":"claude-3","max_tokens":500,"system":"a reasonably long system prompt","messages":[{"role":"user","content":"q"}]}`), &req)

	res := BuildContextAnalysisResult(contextAnalysisInput{
		anthropicReq: &req, maxContext: intPtrBridge(100), maxTokens: 500, modelUsed: "claude-3"})
	if res == nil {
		t.Fatal("result must not be nil")
	}
	if res.SystemTokens == nil || *res.SystemTokens == 0 {
		t.Errorf("system tokens must be set")
	}
	if res.ContextWindow == nil || *res.ContextWindow != 100 {
		t.Errorf("window must be 100")
	}
}

func TestBuildContextAnalysisResult_WindowFallbackToDefaultTable(t *testing.T) {
	// maxContext nil -> fall back to built-in table for modelUsed
	var req domain.OpenAIRequest
	json.Unmarshal([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello world question"}]}`), &req)
	res := BuildContextAnalysisResult(contextAnalysisInput{openaiReq: &req, modelUsed: "gpt-4o", maxTokens: 100})
	if res == nil || res.ContextWindow == nil || *res.ContextWindow != 128000 {
		t.Fatalf("gpt-4o default window must be 128000, got %+v", res.ContextWindow)
	}
}

func TestBuildContextAnalysisResult_NilReq(t *testing.T) {
	if res := BuildContextAnalysisResult(contextAnalysisInput{}); res != nil {
		t.Errorf("nil request -> nil result (unanalyzed path)")
	}
}

func TestBuildContextAnalysisResult_PanicRecovered(t *testing.T) {
	// A panicking observe hook must not escape.
	var r domain.AnthropicRequest
	json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"q"}]}`), &r)
	in := contextAnalysisInput{
		anthropicReq:   &r,
		observeFn:      func(string, int, int) { panic("boom") },
		inputFromUpstr: true, inputTokens: 100,
	}
	if res := BuildContextAnalysisResult(in); res == nil {
		t.Errorf("panic in observe hook must be recovered, analysis result still returned")
	}
}
