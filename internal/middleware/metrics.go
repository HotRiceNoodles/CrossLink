package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_requests_total",
	}, []string{"route_type", "model", "provider", "status_code", "stream"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cl_request_duration_seconds",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"route_type", "model", "provider", "stream"})

	tokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_tokens_total",
	}, []string{"type", "model", "provider"})

	cacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_cache_hits_total",
	}, []string{"model"})

	cacheMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_cache_misses_total",
	}, []string{"model"})

	providerFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_provider_failures_total",
	}, []string{"provider", "error_type"})

	activeRequests = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cl_active_requests",
	}, []string{"route_type"})

	cacheSizeGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cl_cache_size_entries",
		Help: "Current number of cached response entries",
	}, []string{"model"})

	guardrailChecksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_guardrail_checks_total",
	}, []string{"type", "direction", "result"})

	guardrailBlocksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_guardrail_blocks_total",
	}, []string{"type", "model", "action"})

)

// RecordProviderFailure increments the provider failure counter.
func RecordProviderFailure(provider, errorType string) {
	providerFailuresTotal.WithLabelValues(provider, errorType).Inc()
}

// RecordCacheHit increments cache hit counter.
func RecordCacheHit(model string) {
	cacheHitsTotal.WithLabelValues(model).Inc()
}

// RecordCacheMiss increments cache miss counter.
func RecordCacheMiss(model string) {
	cacheMissesTotal.WithLabelValues(model).Inc()
}

// RecordCacheSize updates the cache size gauge.
func RecordCacheSize(n float64) {
	cacheSizeGauge.WithLabelValues("").Set(n)
}

// RecordCacheSizeByModel updates the per-model cache size gauge.
func RecordCacheSizeByModel(model string, n float64) {
	cacheSizeGauge.WithLabelValues(model).Set(n)
}

// RecordGuardrailCheck increments guardrail check counter.
func RecordGuardrailCheck(engineType, direction, result string) {
	guardrailChecksTotal.WithLabelValues(engineType, direction, result).Inc()
}

// RecordGuardrailBlock increments guardrail block counter.
func RecordGuardrailBlock(engineType, model, action string) {
	guardrailBlocksTotal.WithLabelValues(engineType, model, action).Inc()
}

// Metrics returns a gin middleware that records request-level Prometheus metrics.
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		routeType := routeTypeFromPath(path)
		if routeType == "" {
			c.Next()
			return
		}

		activeRequests.WithLabelValues(routeType).Inc()
		defer activeRequests.WithLabelValues(routeType).Dec()

		start := time.Now()
		c.Next()
		duration := time.Since(start).Seconds()

		model, _ := c.Get("model")
		provider, _ := c.Get("provider")
		stream := "false"
		if v, exists := c.Get("stream"); exists {
			if b, ok := v.(bool); ok && b {
				stream = "true"
			}
		}

		status := strconv.Itoa(c.Writer.Status())
		requestsTotal.WithLabelValues(routeType, str(model), str(provider), status, stream).Inc()
		requestDuration.WithLabelValues(routeType, str(model), str(provider), stream).Observe(duration)

		// Record token counts if set by handler
		if inputTokens, exists := c.Get("input_tokens"); exists {
			tokensTotal.WithLabelValues("input", str(model), str(provider)).Add(float64(toInt(inputTokens)))
		}
		if outputTokens, exists := c.Get("output_tokens"); exists {
			tokensTotal.WithLabelValues("output", str(model), str(provider)).Add(float64(toInt(outputTokens)))
		}
	}
}

func routeTypeFromPath(path string) string {
	switch {
	case len(path) >= len("/v1/messages") && path[:len("/v1/messages")] == "/v1/messages":
		return "anthropic"
	case len(path) >= len("/v1/chat/completions") && path[:len("/v1/chat/completions")] == "/v1/chat/completions":
		return "openai"
	case len(path) >= len("/v1/embeddings") && path[:len("/v1/embeddings")] == "/v1/embeddings":
		return "embeddings"
	case len(path) >= len("/admin/api/playground") && path[:len("/admin/api/playground")] == "/admin/api/playground":
		return "playground"
	case len(path) >= 11 && path[:11] == "/v1/images/":
		return "images"
	case len(path) >= 9 && path[:9] == "/v1/audio":
		return "audio"
	case len(path) >= 9 && path[:9] == "/v1/batch":
		return "batch"
	case len(path) >= len("/v1/responses") && path[:len("/v1/responses")] == "/v1/responses":
		return "responses"
	}
	return ""
}

func str(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
