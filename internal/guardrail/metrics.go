package guardrail

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Guardrail Prometheus metrics. Registration lives in this package (next to
// the detection path) because the previous cl_guardrail_* counters declared
// in internal/middleware/metrics.go had no callers — they exported zero
// forever. These replace them.
//
// Label policy: LOW CARDINALITY ONLY. Labels are bounded enums
// (type/direction/result/action/reason). Never an org ID, model name, rule
// name, content fragment or secret. Free-form values (reasons etc.) must not
// reach these labels; log sanitization (SanitizePreview) is unchanged.

var (
	guardrailStreamReleaseSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "cl_guardrail_stream_release_seconds",
		Help:    "Time from receiving a complete SSE frame to releasing that approved frame, including checking and pending predecessors.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5},
	})
	guardrailChecksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_guardrail_checks_total",
		Help: "Guardrail engine checks by engine type, direction and result (hit|miss|error).",
	}, []string{"type", "direction", "result"})

	// guardrailBlocksTotal drops the old `model` label (unbounded-ish and
	// content-adjacent) for cardinality safety; the metric was dead, so no
	// dashboard depends on the old label set.
	guardrailBlocksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_guardrail_blocks_total",
		Help: "Guardrail blocking decisions applied by engine type and action.",
	}, []string{"type", "action"})

	guardrailEngineErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_guardrail_engine_errors_total",
		Help: "Guardrail engine failures (engine creation or engine check error) by engine type. Under engine_error_action=skip these requests are skipped silently at request level — this counter keeps the failure visible.",
	}, []string{"type"})

	guardrailEventsDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_guardrail_events_dropped_total",
		Help: "Guardrail event handler dispatches that did not complete within the 5s budget (reason=timeout) or panicked (reason=panic). SEMANTICS: a drop means the dispatch did not finish in budget — it does NOT prove persistence failed (the handler may have written before the deadline), and a dispatch that finished in budget may still have failed its write (that path logs via slog in the alert service).",
	}, []string{"reason"})

	guardrailRulesLastLoadedSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cl_guardrail_rules_last_loaded_success_timestamp_seconds",
		Help: "Unix timestamp of the last successful rule load on this instance. Staleness = now() - value; the cross-instance propagation bound for a rules change is this age plus the reload TTL (30s default) when the Redis broadcast is unavailable.",
	})
)

func recordCheck(engineType, direction string, hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	guardrailChecksTotal.WithLabelValues(engineType, direction, result).Inc()
}

func recordCheckError(engineType, direction string) {
	guardrailChecksTotal.WithLabelValues(engineType, direction, "error").Inc()
}

func recordBlock(engineType, action string) {
	guardrailBlocksTotal.WithLabelValues(engineType, action).Inc()
}

func recordEngineError(engineType string) {
	guardrailEngineErrorsTotal.WithLabelValues(engineType).Inc()
}

func recordEventDrop(reason string) {
	guardrailEventsDroppedTotal.WithLabelValues(reason).Inc()
}

func recordRulesLoadedSuccess() {
	guardrailRulesLastLoadedSuccess.Set(float64(time.Now().Unix()))
}
