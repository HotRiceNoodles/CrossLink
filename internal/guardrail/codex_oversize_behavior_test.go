package guardrail

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Real-behavior verification for content that exceeds the 500K-rune
// inspection limit (round-10 finding P1-2). These tests pin down what
// actually happens to the unchecked TAIL on both directions:
//
//   - default oversize policy (truncate): the prefix is inspected, the tail
//     is NOT, and every result carries Truncated=true (partial coverage);
//     a marker in the inspected prefix is still detected,
//   - oversize policy reject: the whole request is blocked regardless of
//     where the marker sits, and Truncated is not used to soften it,
//   - strict coverage: an oversize response is rejected even under the
//     default truncate policy.
type oversizeEngine struct{}

func (oversizeEngine) Name() string { return "oversize_marker" }
func (oversizeEngine) Check(_ context.Context, text string, _ Direction, _ string) (*GuardrailResult, error) {
	i := strings.Index(text, "SECRET")
	if i < 0 {
		return &GuardrailResult{}, nil
	}
	return &GuardrailResult{Blocked: true, Reason: "marker in inspected range"}, nil
}

func oversizeBehaviorService(oversize string) *GuardrailService {
	s := NewGuardrailService(nil, nil)
	s.oversizeAction.Store(oversize)
	s.loadedAt = time.Now()
	s.rules = []GuardrailRule{{ID: 8888, Name: "oversize", Type: "oversize_marker", Direction: "both", Severity: "high", Action: "block"}}
	s.engines[8888] = oversizeEngine{}
	return s
}

// oversizeTail builds content longer than maxCheckContent whose SECRET sits
// strictly beyond the inspected prefix.
func oversizeTail() string {
	return strings.Repeat("x", maxCheckContent+11) + "SECRET"
}

func TestCodexOversizeTruncateInspectsPrefixTailUnchecked(t *testing.T) {
	s := oversizeBehaviorService("truncate")
	for _, dir := range []Direction{DirectionRequest, DirectionResponse} {
		res, err := s.Check(context.Background(), &CheckRequest{Content: oversizeTail(), Direction: dir})
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if res.Blocked {
			t.Fatalf("%s: tail-only marker must not claim full-detection block (documented partial coverage)", dir)
		}
		if !res.Truncated {
			t.Fatalf("%s: oversize truncate result missing Truncated flag: %+v", dir, res)
		}
	}
	// Control: the same marker inside the inspected prefix is still detected.
	res, err := s.Check(context.Background(), &CheckRequest{Content: "SECRET" + strings.Repeat("x", maxCheckContent+10), Direction: DirectionRequest})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Blocked {
		t.Fatal("marker inside inspected prefix was not detected")
	}
}

func TestCodexOversizeRejectBlocksTailMarker(t *testing.T) {
	s := oversizeBehaviorService("reject")
	for _, dir := range []Direction{DirectionRequest, DirectionResponse} {
		res, err := s.Check(context.Background(), &CheckRequest{Content: oversizeTail(), Direction: dir})
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if !res.Blocked || res.RuleName != "guardrail_oversize" {
			t.Fatalf("%s: oversize reject must block without inspecting, got %+v", dir, res)
		}
		if res.Truncated {
			t.Fatalf("%s: reject result must not report partial coverage: %+v", dir, res)
		}
	}
}

func TestCodexOversizeStrictCoverageRejectsOversizeResponse(t *testing.T) {
	s := oversizeBehaviorService("truncate")
	s.strictCoverage.Store(true)
	res, err := s.Check(context.Background(), &CheckRequest{Content: oversizeTail(), Direction: DirectionResponse})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Blocked || res.RuleName != "guardrail_oversize" {
		t.Fatalf("strict coverage must reject uninspectable oversize response, got %+v", res)
	}
}
