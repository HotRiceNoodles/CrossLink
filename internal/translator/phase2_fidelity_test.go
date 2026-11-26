package translator

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
)

// Phase-2 fidelity (2026-11-26 checklist §3/§4): refusal stop-reason round
// trip and verbatim passthrough of unrecognized content blocks.

func TestRefusalRoundTrip(t *testing.T) {
	t.Parallel()
	// Anthropic upstream refused → internal finish_reason → back to anthropic.
	anthropicResp := &domain.AnthropicResponse{
		ID: "m1", Type: "message", Role: "assistant", Model: "m",
		Content:    []domain.ContentBlock{{Type: "text", Text: ""}},
		StopReason: "refusal",
		Usage:      domain.AnthropicUsage{InputTokens: 1, OutputTokens: 1},
	}
	internal, err := AnthropicToOpenAIResponse(anthropicResp)
	if err != nil {
		t.Fatal(err)
	}
	if got := internal.Choices[0].FinishReason; got != "content_filter" {
		t.Fatalf("refusal → %q, want content_filter", got)
	}
	back, err := OpenAIToAnthropic(internal, "m")
	if err != nil {
		t.Fatal(err)
	}
	if back.StopReason != "refusal" {
		t.Fatalf("content_filter → %q, want refusal (round trip must not degrade to end_turn)", back.StopReason)
	}
}

func TestUnknownBlockVerbatimRoundTrip(t *testing.T) {
	t.Parallel()
	rawBlock := `{"type":"compaction_block","compaction":{"summary":"...","replacement_context":"..."}}`
	var block domain.ContentBlock
	if err := json.Unmarshal([]byte(rawBlock), &block); err != nil {
		t.Fatal(err)
	}
	if len(block.Raw) == 0 {
		t.Fatal("unrecognized block type must stash Raw bytes")
	}

	anthropicResp := &domain.AnthropicResponse{
		ID: "m1", Type: "message", Role: "assistant", Model: "m",
		Content: []domain.ContentBlock{
			{Type: "text", Text: "hi"},
			block,
		},
		StopReason: "end_turn",
		Usage:      domain.AnthropicUsage{InputTokens: 1, OutputTokens: 1},
	}
	internal, err := AnthropicToOpenAIResponse(anthropicResp)
	if err != nil {
		t.Fatal(err)
	}
	if len(internal.AnthropicExtraBlocks) != 1 {
		t.Fatalf("extra blocks carried = %d, want 1", len(internal.AnthropicExtraBlocks))
	}
	// json:"-" keeps the carry off the openai wire.
	wire, _ := json.Marshal(internal)
	var probe map[string]any
	if err := json.Unmarshal(wire, &probe); err != nil {
		t.Fatal(err)
	}
	for k := range probe {
		if k == "AnthropicExtraBlocks" || k == "anthropic_extra_blocks" {
			t.Fatalf("carry leaked onto openai wire as %q", k)
		}
	}

	back, err := OpenAIToAnthropic(internal, "m")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range back.Content {
		if b.Type == "compaction_block" {
			out, _ := json.Marshal(b)
			if string(out) != rawBlock {
				t.Fatalf("verbatim mismatch:\n got %s\nwant %s", out, rawBlock)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("compaction block lost on round trip: %+v", back.Content)
	}
	// Recognized blocks still marshal through typed fields.
	if back.Content[0].Type != "text" || back.Content[0].Text != "hi" {
		t.Fatalf("recognized block degraded: %+v", back.Content[0])
	}
}

func TestKnownBlockMarshalUnchanged(t *testing.T) {
	t.Parallel()
	b := domain.ContentBlock{Type: "text", Text: "hello"}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"text","text":"hello"}`
	if string(out) != want {
		t.Fatalf("known block marshal = %s, want %s", out, want)
	}
	var back domain.ContentBlock
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back.Raw != nil {
		t.Fatal("known type must not stash Raw")
	}
}

// Request-side carry (Phase-2 compact 前置): a client replaying a compaction
// block in an assistant message survives the gateway round trip verbatim.
func TestUnknownBlockRequestRoundTrip(t *testing.T) {
	t.Parallel()
	rawBlock := `{"type":"compaction_block","compaction":{"summary":"s","replacement_context":"c"}}`
	req := &domain.AnthropicRequest{
		Model:     "m",
		MaxTokens: 64,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"a"},` + rawBlock + `]`)},
			{Role: "user", Content: json.RawMessage(`"go on"`)},
		},
	}
	internal, err := AnthropicToOpenAI(req, "")
	if err != nil {
		t.Fatal(err)
	}
	var carried int
	for _, m := range internal.Messages {
		carried += len(m.UnknownBlocks)
	}
	if carried != 1 {
		t.Fatalf("unknown blocks carried = %d, want 1", carried)
	}

	back, err := OpenAIToAnthropicRequest(internal)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range back.Messages {
		var blocks []domain.ContentBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "compaction_block" {
				out, _ := json.Marshal(b)
				if string(out) != rawBlock {
					t.Fatalf("request round trip mismatch:\n got %s\nwant %s", out, rawBlock)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("compaction block lost on request round trip")
	}
}

// usage.iterations (Phase-2 compact 前置) survives the response round trip.
func TestUsageIterationsRoundTrip(t *testing.T) {
	t.Parallel()
	anthropicResp := &domain.AnthropicResponse{
		ID: "m1", Type: "message", Role: "assistant", Model: "m",
		Content:    []domain.ContentBlock{{Type: "text", Text: "ok"}},
		StopReason: "end_turn",
		Usage:      domain.AnthropicUsage{InputTokens: 1, OutputTokens: 2, Iterations: 3},
	}
	internal, err := AnthropicToOpenAIResponse(anthropicResp)
	if err != nil {
		t.Fatal(err)
	}
	if internal.Usage.AnthropicIterations != 3 {
		t.Fatalf("iterations carry = %d, want 3", internal.Usage.AnthropicIterations)
	}
	wire, _ := json.Marshal(internal)
	if bytesContains(string(wire), "iterations") {
		t.Fatal("iterations must not leak onto the openai wire")
	}
	back, err := OpenAIToAnthropic(internal, "m")
	if err != nil {
		t.Fatal(err)
	}
	if back.Usage.Iterations != 3 {
		t.Fatalf("iterations round trip = %d, want 3", back.Usage.Iterations)
	}
}

func bytesContains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

// Stream passthrough (Phase-2 compact 前置, the last blocker): a compaction
// block's start/delta/stop survive the double translation verbatim, and a
// locally-generated text block afterwards does not collide with its index.
func TestUnknownBlockStreamPassthrough(t *testing.T) {
	t.Parallel()
	rev := NewReverseStreamTranslator()
	fwd := NewStreamTranslator("msg_t", "m", 0)

	var out []domain.SSEEvent
	translate := func(event string, data string) {
		for _, ch := range rev.TranslateEvent(event, []byte(data)) {
			if ch.Chunk == nil {
				continue // Done marker
			}
			out = append(out, fwd.TranslateChunk(ch)...)
		}
	}

	translate("message_start", `{"type":"message_start","message":{"id":"m1","model":"m","usage":{"input_tokens":1}}}`)
	// text block (index 0), then compaction block (index 1) with one delta.
	translate("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`)
	translate("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"hi"}}`)
	compStart := `{"index":1,"content_block":{"type":"compaction_block","compaction":{"summary":"s"}}}`
	translate("content_block_start", compStart)
	compDelta := `{"index":1,"delta":{"type":"compaction_delta","payload":"x"}}`
	translate("content_block_delta", compDelta)
	compStop := `{"index":1}`
	translate("content_block_stop", compStop)
	// text block AFTER the compaction block must not reuse index 1.
	translate("content_block_delta", `{"index":2,"delta":{"type":"text_delta","text":"tail"}}`)

	joined := eventDump(out)
	for _, want := range []string{compStart, compDelta, `"event":"content_block_stop"`} {
		if !bytesContains(joined, want) && want != `"event":"content_block_stop"` {
			t.Fatalf("passthrough missing %s in\n%s", want, joined)
		}
	}
	// The compaction start/delta bytes ride verbatim.
	if !bytesContains(joined, `"type":"compaction_block"`) || !bytesContains(joined, `"compaction_delta"`) {
		t.Fatalf("compaction events lost in\n%s", joined)
	}
	// The post-compaction text block uses a fresh index (no collision with 1).
	if bytesContains(joined, `"index":1,"delta":{"type":"text_delta"`) {
		t.Fatalf("index collision after compaction block in\n%s", joined)
	}
}

func eventDump(events []domain.SSEEvent) string {
	var b []byte
	for _, e := range events {
		b = append(b, e.Data...)
	}
	return string(b)
}
