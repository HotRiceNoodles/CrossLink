package router

import (
	"context"
	"math/rand/v2"
	"sort"
)

type WeightedRandomStrategy struct {
	healthFn HealthScoreFn // optional (P4); nil = health-unaware (pre-P4 behavior)
}

// SetHealthScoreFn injects an optional health-score provider (P4, v5.1 D-1).
// When set, candidates with score 0 are demoted to the fallback pool (via
// filterByHealth) and the primary is picked by effective weight = Weight × score.
func (s *WeightedRandomStrategy) SetHealthScoreFn(fn HealthScoreFn) { s.healthFn = fn }

func (s *WeightedRandomStrategy) Name() StrategyName { return StrategyWeightedRandom }

func (s *WeightedRandomStrategy) Select(_ context.Context, candidates []RouteCandidate) (*RouteCandidate, []*RouteResult) {
	primaries, fallbacks := splitPrimariesFallbacks(candidates)
	// P4: demote health=0 primaries into fallback (count-only guard flags,
	// circuit-open, error-rate → 0). If healthFn is nil this is a no-op.
	primaries, fallbacks = filterByHealth(s.healthFn, primaries, fallbacks)
	if len(primaries) == 0 {
		return nil, candidatesToRouteResults(fallbacks)
	}

	picked := weightedPickByEffective(primaries, s.healthFn)
	ordered := []*RouteResult{candidateToRouteResult(*picked)}

	// Remaining primaries ordered by effective weight DESC, then Priority ASC
	// (粗糙点 A: Priority finally takes effect as the fallback-chain tiebreaker).
	remaining := make([]RouteCandidate, 0, len(primaries)-1)
	for _, p := range primaries {
		if p.ModelID != picked.ModelID {
			remaining = append(remaining, p)
		}
	}
	sort.SliceStable(remaining, func(i, j int) bool {
		ewi := effectiveWeight(remaining[i], s.healthFn)
		ewj := effectiveWeight(remaining[j], s.healthFn)
		if ewi != ewj {
			return ewi > ewj
		}
		return remaining[i].Priority < remaining[j].Priority
	})
	ordered = append(ordered, candidatesToRouteResults(remaining)...)
	ordered = append(ordered, candidatesToRouteResults(fallbacks)...)
	return picked, ordered
}

// weightedPickCandidates picks by raw Weight. Used by weighted_random when no
// healthFn is wired AND by the overlay canary strategy — signature/semantics
// unchanged (P4-1: overlay depends on it).
func weightedPickCandidates(candidates []RouteCandidate) *RouteCandidate {
	if len(candidates) == 0 {
		return nil
	}
	totalWeight := 0
	for _, c := range candidates {
		totalWeight += c.Weight
	}
	if totalWeight == 0 {
		return &candidates[0]
	}
	rng := rand.IntN(totalWeight)
	running := 0
	for i := range candidates {
		running += candidates[i].Weight
		if rng < running {
			return &candidates[i]
		}
	}
	return &candidates[0]
}

// effectiveWeight returns Weight × healthScore (or just Weight when healthFn is
// nil / unavailable). Used by weighted_random's P4 effective-weight selection.
func effectiveWeight(c RouteCandidate, healthFn HealthScoreFn) float64 {
	if healthFn == nil {
		return float64(c.Weight)
	}
	return float64(c.Weight) * healthFn(c.Provider.Name(), c.ProviderModel)
}

// weightedPickByEffective picks a primary by effective weight (Weight × health).
// Used only by weighted_random's Select (P4); weightedPickCandidates is
// unchanged for canary. Expects candidates already health-filtered (score > 0),
// but degrades safely to raw Weight if all effective weights are 0.
func weightedPickByEffective(candidates []RouteCandidate, healthFn HealthScoreFn) *RouteCandidate {
	if len(candidates) == 0 {
		return nil
	}
	ews := make([]float64, len(candidates))
	total := 0.0
	for i := range candidates {
		ews[i] = effectiveWeight(candidates[i], healthFn)
		total += ews[i]
	}
	if total <= 0 {
		return &candidates[0]
	}
	rng := rand.Float64() * total
	running := 0.0
	for i := range candidates {
		running += ews[i]
		if rng < running {
			return &candidates[i]
		}
	}
	return &candidates[0]
}
