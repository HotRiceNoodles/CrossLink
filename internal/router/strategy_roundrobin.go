package router

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type RoundRobinStrategy struct {
	rdb      *redis.Client
	healthFn HealthScoreFn // optional (P4); nil = health-unaware
}

func NewRoundRobinStrategy(rdb *redis.Client) *RoundRobinStrategy {
	return &RoundRobinStrategy{rdb: rdb}
}

// SetHealthScoreFn injects an optional health-score provider (P4). When set,
// health=0 primaries are demoted to fallback before the modulo pick (so the RR
// counter only cycles among healthy providers). Modulo skew when the healthy
// set oscillates is a known compromise (design §4.4 P4-2).
func (s *RoundRobinStrategy) SetHealthScoreFn(fn HealthScoreFn) { s.healthFn = fn }

func (s *RoundRobinStrategy) Name() StrategyName { return StrategyRoundRobin }

func (s *RoundRobinStrategy) Select(ctx context.Context, candidates []RouteCandidate) (*RouteCandidate, []*RouteResult) {
	primaries, fallbacks := splitPrimariesFallbacks(candidates)
	// P4: demote health=0 primaries (no-op when healthFn is nil).
	primaries, fallbacks = filterByHealth(s.healthFn, primaries, fallbacks)
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
