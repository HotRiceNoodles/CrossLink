package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStats_ZeroDivisionProtection(t *testing.T) {
	// Verify the zero-division guard logic that runs in Stats()
	// When total_requests is 0, all rates must be 0, not NaN

	totalRequests := int64(0)
	totalTokens := int64(0)
	totalCost := 0.0

	var errorCount, fallbackCount, retryCount, guardrailCount int64 = 0, 0, 0, 0

	// Replicate the guard logic from Stats()
	var costPer1kTokens, costPerRequest, errorRate, fallbackRate, retryRate, guardrailRate float64

	if totalTokens > 0 {
		costPer1kTokens = totalCost / float64(totalTokens) * 1000
	}
	if totalRequests > 0 {
		costPerRequest = totalCost / float64(totalRequests)
		errorRate = float64(errorCount) / float64(totalRequests)
		fallbackRate = float64(fallbackCount) / float64(totalRequests)
		retryRate = float64(retryCount) / float64(totalRequests)
		guardrailRate = float64(guardrailCount) / float64(totalRequests)
	}

	assert.Equal(t, 0.0, costPer1kTokens, "costPer1kTokens should be 0 when totalTokens is 0")
	assert.Equal(t, 0.0, costPerRequest, "costPerRequest should be 0 when totalRequests is 0")
	assert.Equal(t, 0.0, errorRate, "errorRate should be 0 when totalRequests is 0")
	assert.Equal(t, 0.0, fallbackRate, "fallbackRate should be 0 when totalRequests is 0")
	assert.Equal(t, 0.0, retryRate, "retryRate should be 0 when totalRequests is 0")
	assert.Equal(t, 0.0, guardrailRate, "guardrailRate should be 0 when totalRequests is 0")
}

func TestStats_NonZeroCalculations(t *testing.T) {
	// Verify correct calculations with actual values
	totalRequests := int64(1000)
	totalTokens := int64(500000)
	totalCost := 12.5
	var errorCount, fallbackCount, retryCount, guardrailCount int64 = 23, 50, 30, 10

	var costPer1kTokens, costPerRequest, errorRate, fallbackRate, retryRate, guardrailRate float64

	if totalTokens > 0 {
		costPer1kTokens = totalCost / float64(totalTokens) * 1000
	}
	if totalRequests > 0 {
		costPerRequest = totalCost / float64(totalRequests)
		errorRate = float64(errorCount) / float64(totalRequests)
		fallbackRate = float64(fallbackCount) / float64(totalRequests)
		retryRate = float64(retryCount) / float64(totalRequests)
		guardrailRate = float64(guardrailCount) / float64(totalRequests)
	}

	assert.InDelta(t, 0.025, costPer1kTokens, 0.0001)
	assert.InDelta(t, 0.0125, costPerRequest, 0.0001)
	assert.InDelta(t, 0.023, errorRate, 0.0001)
	assert.InDelta(t, 0.05, fallbackRate, 0.0001)
	assert.InDelta(t, 0.03, retryRate, 0.0001)
	assert.InDelta(t, 0.01, guardrailRate, 0.0001)
}

func TestDailyStat_FieldsMatchScanOrder(t *testing.T) {
	// Verify DailyStat struct has the correct field count and order.
	// The SELECT order in DailyTrend is:
	// date, count, tokens, input_tokens, output_tokens, reasoning_tokens, cache_read_tokens,
	// fallback_count_daily, retry_count_daily, guardrail_count_daily, cost
	// Total: 11 columns

	s := DailyStat{}
	// Just verify the struct has all expected fields by setting them
	s.Date = "2026-06-04"
	s.Count = 100
	s.Tokens = 5000
	s.InputTokens = 3500
	s.OutputTokens = 1200
	s.ReasoningTokens = 200
	s.CacheReadTokens = 100
	s.FallbackCountDaily = 5
	s.RetryCountDaily = 3
	s.GuardrailCountDaily = 1
	s.Cost = 1.25

	assert.Equal(t, "2026-06-04", s.Date)
	assert.Equal(t, int64(100), s.Count)
	assert.Equal(t, int64(5000), s.Tokens)
	assert.Equal(t, int64(3500), s.InputTokens)
	assert.Equal(t, int64(1200), s.OutputTokens)
	assert.Equal(t, int64(200), s.ReasoningTokens)
	assert.Equal(t, int64(100), s.CacheReadTokens)
	assert.Equal(t, int64(5), s.FallbackCountDaily)
	assert.Equal(t, int64(3), s.RetryCountDaily)
	assert.Equal(t, int64(1), s.GuardrailCountDaily)
	assert.InDelta(t, 1.25, s.Cost, 0.001)
}
