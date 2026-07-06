package router

// HealthScoreFn returns a health score in [0, 1] for a (provider, model).
// 0 means the provider should be demoted out of the primary pool (it is
// rate-limited, over budget, circuit-open, or erroring heavily).
//
// nil means "no health provider wired" — strategies treat it as opt-out and
// behave exactly as before (gradual enhancement, not forced; design §6).
//
// router cannot import service (cycle), so the health-score dependency is
// injected as a function value. The commercial build wires a closure over
// service.HealthScore; Community wires it in P4.5 via Resolver.SetHealthScoreFn.
type HealthScoreFn func(providerName, model string) float64

// filterByHealth demotes primaries whose health score is 0 into the fallback
// pool. If healthFn is nil or EVERY primary would be demoted, primaries are
// returned unchanged — the latter so the strategy can still pick the best
// unhealthy option rather than returning an empty primary set (which would
// collapse to fallback-only and likely 503).
//
// Shared by Community's weighted_random/round_robin (P4) and the commercial
// overlay's least_cost/least_latency/least_busy/canary (B2) so neither
// re-implements the demote logic. Promoted from overlay in P4.3 (v5.1 D-1).
func filterByHealth(healthFn HealthScoreFn, primaries, fallbacks []RouteCandidate) ([]RouteCandidate, []RouteCandidate) {
	if healthFn == nil || len(primaries) == 0 {
		return primaries, fallbacks
	}
	healthy := make([]RouteCandidate, 0, len(primaries))
	var demoted []RouteCandidate
	for _, p := range primaries {
		if healthFn(p.Provider.Name(), p.ProviderModel) > 0 {
			healthy = append(healthy, p)
		} else {
			demoted = append(demoted, p)
		}
	}
	if len(healthy) == 0 {
		// All unhealthy: keep originals (don't pollute fallbacks with copies).
		return primaries, fallbacks
	}
	return healthy, append(fallbacks, demoted...)
}
