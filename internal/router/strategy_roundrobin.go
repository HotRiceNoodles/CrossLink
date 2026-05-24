package router

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type RoundRobinStrategy struct {
	rdb *redis.Client
}

func NewRoundRobinStrategy(rdb *redis.Client) *RoundRobinStrategy {
	return &RoundRobinStrategy{rdb: rdb}
}

func (s *RoundRobinStrategy) Name() StrategyName { return StrategyRoundRobin }

func (s *RoundRobinStrategy) Select(ctx context.Context, candidates []RouteCandidate) (*RouteCandidate, []*RouteResult) {
	primaries, fallbacks := splitPrimariesFallbacks(candidates)
	if len(primaries) == 0 {
		return nil, candidatesToRouteResults(fallbacks)
	}

	if s.rdb == nil {
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

	modelName := primaries[0].ModelName
	idx, err := s.rdb.Incr(ctx, "rr:"+modelName).Result()
	if err != nil {
		slog.Warn("round-robin redis incr failed, falling back to weighted random", "error", err)
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
	// Auto-clean stale counters after 24h
	s.rdb.Expire(ctx, "rr:"+modelName, 24*time.Hour)

	pickedIdx := int(idx) % len(primaries)
	picked := primaries[pickedIdx]

	ordered := []*RouteResult{candidateToRouteResult(picked)}
	for i, p := range primaries {
		if i == pickedIdx {
			continue
		}
		ordered = append(ordered, candidateToRouteResult(p))
	}
	ordered = append(ordered, candidatesToRouteResults(fallbacks)...)
	return &picked, ordered
}
