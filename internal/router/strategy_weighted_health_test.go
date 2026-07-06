package router

import (
	"context"
	"testing"
)

func mkWeightCandidate(name, model string, weight, priority int, modelID int64) RouteCandidate {
	return RouteCandidate{
		Provider:      &mockProvider{name: name},
		ProviderModel: model,
		Weight:        weight,
		Priority:      priority,
		ModelID:       modelID,
	}
}

// When healthFn is nil, weighted_random behaves as before (raw-weight pick).
func TestWeightedRandom_NilHealthFnIsNoOp(t *testing.T) {
	s := &WeightedRandomStrategy{}
	candidates := []RouteCandidate{
		mkWeightCandidate("A", "m", 10, 1, 1),
		mkWeightCandidate("B", "m", 10, 1, 2),
	}
	picked, ordered := s.Select(context.Background(), candidates)
	if picked == nil {
		t.Fatal("expected a pick")
	}
	if len(ordered) != 2 {
		t.Errorf("expected 2 ordered, got %d", len(ordered))
	}
}

// A health=0 provider is demoted out of the primary pool: it is never picked
// as the primary and appears at the end of the ordered list (fallback).
func TestWeightedRandom_HealthZeroDemoted(t *testing.T) {
	s := &WeightedRandomStrategy{healthFn: func(name, _ string) float64 {
		if name == "B" {
			return 0
		}
		return 1.0
	}}
	candidates := []RouteCandidate{
		mkWeightCandidate("A", "m", 5, 1, 1),
		mkWeightCandidate("B", "m", 5, 1, 2), // health 0 → demoted
	}
	picked, ordered := s.Select(context.Background(), candidates)
	if picked.Provider.Name() != "A" {
		t.Errorf("expected A picked (B demoted), got %s", picked.Provider.Name())
	}
	// ordered = [A] + [] + [B fallback] → last must be B
	if ordered[len(ordered)-1].Provider.Name() != "B" {
		t.Errorf("expected B last (fallback), got %s", ordered[len(ordered)-1].Provider.Name())
	}
}

// Soft de-weight: a half-health provider is picked less often than a full-health
// one with equal weight. Statistical check over many iterations.
func TestWeightedRandom_SoftDeweight(t *testing.T) {
	s := &WeightedRandomStrategy{healthFn: func(name, _ string) float64 {
		if name == "B" {
			return 0.5 // half effective weight
		}
		return 1.0
	}}
	candidates := []RouteCandidate{
		mkWeightCandidate("A", "m", 10, 1, 1), // eff 10
		mkWeightCandidate("B", "m", 10, 1, 2), // eff 5
	}
	aPicks, bPicks := 0, 0
	for i := 0; i < 2000; i++ {
		picked, _ := s.Select(context.Background(), candidates)
		if picked.Provider.Name() == "A" {
			aPicks++
		} else {
			bPicks++
		}
	}
	// Expect A:B ≈ 10:5 = 2:1. Allow generous tolerance (random).
	if aPicks < bPicks {
		t.Errorf("A (eff 10) should out-pick B (eff 5): A=%d B=%d", aPicks, bPicks)
	}
	ratio := float64(aPicks) / float64(bPicks)
	if ratio < 1.5 || ratio > 2.5 {
		t.Errorf("expected A/B ratio ~2.0, got %.2f (A=%d B=%d)", ratio, aPicks, bPicks)
	}
}

// Priority is the fallback-chain tiebreaker when effective weights tie.
// With 3 equal-weight primaries, whichever is picked, the remaining two must
// be ordered by Priority ASC.
func TestWeightedRandom_PriorityTiebreaker(t *testing.T) {
	s := &WeightedRandomStrategy{healthFn: func(_, _ string) float64 { return 1.0 }}
	candidates := []RouteCandidate{
		mkWeightCandidate("A", "m", 10, 5, 1),
		mkWeightCandidate("B", "m", 10, 1, 2),
		mkWeightCandidate("C", "m", 10, 3, 3),
	}
	for i := 0; i < 50; i++ {
		_, ordered := s.Select(context.Background(), candidates)
		// ordered[0] = picked; ordered[1],[2] = remaining primaries (Priority ASC).
		if ordered[1].Priority > ordered[2].Priority {
			t.Errorf("expected remaining primaries Priority ASC, got %d before %d", ordered[1].Priority, ordered[2].Priority)
		}
	}
}
