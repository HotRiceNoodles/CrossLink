package router

import (
	"context"
	"math/rand/v2"
)

type WeightedRandomStrategy struct{}

func (s *WeightedRandomStrategy) Name() StrategyName { return StrategyWeightedRandom }

func (s *WeightedRandomStrategy) Select(_ context.Context, candidates []RouteCandidate) (*RouteCandidate, []*RouteResult) {
	primaries, fallbacks := splitPrimariesFallbacks(candidates)
	if len(primaries) == 0 {
		return nil, candidatesToRouteResults(fallbacks)
	}

	picked := weightedPickCandidates(primaries)

	ordered := []*RouteResult{candidateToRouteResult(*picked)}
	for _, p := range primaries {
		if p.ModelID == picked.ModelID {
			continue
		}
		ordered = append(ordered, candidateToRouteResult(p))
	}
	ordered = append(ordered, candidatesToRouteResults(fallbacks)...)
	return picked, ordered
}

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
