package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeRoutingDistribution_BasicSplit(t *testing.T) {
	// provider A weight 7, provider B weight 3
	// actual: A=70 requests (7 errors), B=30 requests (0 errors)
	cfg := []providerConfigWeight{
		{ProviderID: 1, Weight: 7, DisplayName: "A"},
		{ProviderID: 2, Weight: 3, DisplayName: "B"},
	}
	actual := map[int64]providerActual{
		1: {Requests: 70, Errors: 7, AvgLatencyMs: 100, Tokens: 1000, Cost: 1.5},
		2: {Requests: 30, Errors: 0, AvgLatencyMs: 200, Tokens: 500, Cost: 0.5},
	}
	rows := computeRoutingDistribution(cfg, actual)

	assert.Len(t, rows, 2)
	// configured: total weight 10 -> A 70%, B 30%
	assert.InDelta(t, 0.7, rows[0].ConfigWeightPct, 1e-9)
	assert.InDelta(t, 0.3, rows[1].ConfigWeightPct, 1e-9)
	// actual: A 70/100=0.7, B 30/100=0.3
	assert.InDelta(t, 0.7, rows[0].ActualPct, 1e-9)
	assert.InDelta(t, 0.0, rows[0].Deviation, 1e-9) // A perfectly balanced
	assert.InDelta(t, 0.1, rows[0].ErrorRate, 1e-9) // 7/70
}

func TestComputeRoutingDistribution_IncludesZeroTrafficProvider(t *testing.T) {
	cfg := []providerConfigWeight{
		{ProviderID: 1, Weight: 5, DisplayName: "A"},
		{ProviderID: 2, Weight: 5, DisplayName: "B"}, // configured but unused
	}
	actual := map[int64]providerActual{
		1: {Requests: 100, Errors: 0, AvgLatencyMs: 50, Tokens: 0, Cost: 0},
	}
	rows := computeRoutingDistribution(cfg, actual)
	assert.Len(t, rows, 2)

	var b *routingDistRow
	for i := range rows {
		if rows[i].ProviderID == 2 {
			b = &rows[i]
		}
	}
	if b == nil {
		t.Fatal("expected provider B (configured, zero traffic) in results")
	}
	assert.Equal(t, int64(0), b.Requests)
	assert.InDelta(t, 0.5, b.ConfigWeightPct, 1e-9)
	assert.InDelta(t, 0.0, b.ActualPct, 1e-9)
	assert.InDelta(t, -0.5, b.Deviation, 1e-9) // configured 50% but got 0%
	assert.InDelta(t, 0.0, b.ErrorRate, 1e-9)  // 0/0 -> 0, not NaN
}

func TestComputeRoutingDistribution_ZeroDivisionGuard(t *testing.T) {
	// no config, no traffic -> empty
	assert.Empty(t, computeRoutingDistribution(nil, nil))

	// config with zero total weight + zero traffic -> all pct 0, no NaN
	cfg := []providerConfigWeight{{ProviderID: 1, Weight: 0, DisplayName: "X"}}
	actual := map[int64]providerActual{1: {Requests: 0}}
	rows := computeRoutingDistribution(cfg, actual)
	assert.Len(t, rows, 1)
	assert.InDelta(t, 0.0, rows[0].ConfigWeightPct, 1e-9)
	assert.InDelta(t, 0.0, rows[0].ActualPct, 1e-9)
	assert.InDelta(t, 0.0, rows[0].Deviation, 1e-9)
	assert.InDelta(t, 0.0, rows[0].ErrorRate, 1e-9)
}

func TestComputeRoutingDistribution_ActualWithNoConfig(t *testing.T) {
	// traffic to a provider with no weight config (orphan)
	cfg := []providerConfigWeight{}
	actual := map[int64]providerActual{
		99: {Requests: 50, Errors: 5, AvgLatencyMs: 80, Tokens: 100, Cost: 0.2},
	}
	rows := computeRoutingDistribution(cfg, actual)
	assert.Len(t, rows, 1)
	assert.Equal(t, int64(99), rows[0].ProviderID)
	assert.Equal(t, 0, rows[0].ConfigWeight)
	assert.InDelta(t, 1.0, rows[0].ActualPct, 1e-9) // 100% of traffic
	assert.InDelta(t, 0.1, rows[0].ErrorRate, 1e-9) // 5/50
}

func TestComputeRoutingDistribution_OrphansStillGetConfigIfPresent(t *testing.T) {
	// If a provider appears in both cfg and actual, emit once with both.
	cfg := []providerConfigWeight{{ProviderID: 1, Weight: 10, DisplayName: "A"}}
	actual := map[int64]providerActual{1: {Requests: 5}}
	rows := computeRoutingDistribution(cfg, actual)
	assert.Len(t, rows, 1, "provider in both cfg and actual must not be double-counted")
	assert.Equal(t, int64(1), rows[0].ProviderID)
	assert.Equal(t, int64(5), rows[0].Requests)
	assert.Equal(t, 10, rows[0].ConfigWeight)
}
