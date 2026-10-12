package guardrail

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
)

// A shorter match must never discard the exposed tail of a longer overlapping
// match from another masking rule. This is independent security acceptance.
func TestCodexAcceptanceMaskOverlapDoesNotExposeTail(t *testing.T) {
	content := "prefix-ABCDEFGHIJ-suffix"
	spans := []Span{
		{Start: 7, End: 11, Replacement: "[SHORT]"},
		{Start: 9, End: 17, Replacement: "[LONG]"},
	}
	got := applySpans(content, composeSpans(spans))
	if strings.Contains(got, "EFGHIJ") {
		t.Fatalf("overlapping mask discarded a sensitive tail: %q", got)
	}
}

type codexAcceptanceEngine struct{ mask bool }

func (e codexAcceptanceEngine) Name() string { return "codex_acceptance" }
func (e codexAcceptanceEngine) Check(_ context.Context, text string, _ Direction, _ string) (*GuardrailResult, error) {
	i := strings.Index(text, "SECRET")
	if i < 0 {
		return &GuardrailResult{}, nil
	}
	r := &GuardrailResult{Blocked: true, Reason: "synthetic sensitive marker"}
	if e.mask {
		r.MaskedContent = strings.ReplaceAll(text, "SECRET", "[MASKED]")
		r.MaskedSpans = []Span{{Start: i, End: i + 6, Replacement: "[MASKED]"}}
	}
	return r, nil
}

func codexAcceptanceStrictService(mask bool) *GuardrailService {
	s := NewGuardrailService(nil, nil)
	s.streamMode.Store("strict")
	s.loadedAt = time.Now()
	action := "block"
	if mask {
		action = "mask"
	}
	s.rules = []GuardrailRule{{ID: 9999, Name: "acceptance", Type: "codex_acceptance", Direction: "response", Severity: "high", Action: action}}
	s.engines[9999] = codexAcceptanceEngine{mask: mask}
	return s
}

func codexAcceptanceChunk(index int, text string) domain.SSEChunk {
	return domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Index: index, Delta: domain.OpenAIChunkDelta{Content: text}}}}}
}

func TestCodexAcceptanceStrictStreamAppliesMask(t *testing.T) {
	s := codexAcceptanceStrictService(true)
	ch := make(chan domain.SSEChunk, 2)
	ch <- codexAcceptanceChunk(0, "SECRET")
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	w := NewStreamGuardrailWrapper(ch, s, "", 1, 1, 1)
	r := w.Next(context.Background())
	if r.Chunk == nil || r.Chunk.Chunk == nil {
		t.Fatalf("expected safely masked content, got %#v", r)
	}
	got := r.Chunk.Chunk.Choices[0].Delta.Content
	if strings.Contains(got, "SECRET") {
		t.Fatalf("strict stream emitted original mask-action content: %q", got)
	}
}

func TestCodexAcceptanceStrictStreamReassemblesInterleavedChoices(t *testing.T) {
	s := codexAcceptanceStrictService(false)
	ch := make(chan domain.SSEChunk, 4)
	ch <- codexAcceptanceChunk(0, "SE")
	ch <- codexAcceptanceChunk(1, "unrelated")
	ch <- codexAcceptanceChunk(0, "CRET")
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	w := NewStreamGuardrailWrapper(ch, s, "", 1, 1, 1)
	r := w.Next(context.Background())
	if r.Blocked == nil {
		t.Fatalf("interleaved choices evaded strict detection: %#v", r)
	}
}

func TestCodexAcceptanceStrictStreamCapsNonTextBytes(t *testing.T) {
	s := codexAcceptanceStrictService(false)
	s.strictMaxBytes.Store(512)
	ch := make(chan domain.SSEChunk, 2)
	ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{ID: strings.Repeat("x", 1024)}}
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	w := NewStreamGuardrailWrapper(ch, s, "", 1, 1, 1)
	if r := w.Next(context.Background()); r.Blocked == nil {
		t.Fatalf("non-text buffered bytes bypassed strict cap: %#v", r)
	}
}

func TestCodexAcceptanceStrictStreamChecksDecodedToolArguments(t *testing.T) {
	s := codexAcceptanceStrictService(false)
	ch := make(chan domain.SSEChunk, 2)
	ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Index: 0, Delta: domain.OpenAIChunkDelta{ToolCalls: []domain.OpenAIChunkToolCall{{Index: 0, Function: domain.OpenAIChunkFunctionCall{Arguments: `{"query":"SE\u0043RET"}`}}}}}}}}
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	w := NewStreamGuardrailWrapper(ch, s, "", 1, 1, 1)
	if r := w.Next(context.Background()); r.Blocked == nil {
		t.Fatalf("decoded stream tool argument bypassed detection: %#v", r)
	}
}

func TestCodexAcceptanceMaskContainedMatchDoesNotExposeOuterTail(t *testing.T) {
	content := "ABCDEFGHIJ"
	spans := []Span{
		{Start: 0, End: 3, Replacement: "[SHORT]"},
		{Start: 0, End: 10, Replacement: "[LONG]"},
	}
	got := applySpans(content, composeSpans(spans))
	if strings.Contains(got, "DEFGHIJ") {
		t.Fatalf("shorter span exposed enclosing sensitive match: %q", got)
	}
}
