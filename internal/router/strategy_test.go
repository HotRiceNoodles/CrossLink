package router

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeCandidate(provider string, weight, priority int, extras ...func(*RouteCandidate)) RouteCandidate {
	c := RouteCandidate{
		Provider:      &mockProvider{name: provider},
		ProviderModel: provider + "-model",
		ProviderRow:   &model.Provider{Name: provider, Status: 1},
		Weight:        weight,
		Priority:      priority,
		ModelID:       int64(hashProvider(provider)),
		ModelName:     "test-model",
		InputPrice:    0.001,
		OutputPrice:   0.002,
		Currency:      "CNY",
	}
	for _, fn := range extras {
		fn(&c)
	}
	return c
}

func hashProvider(name string) int {
	h := 0
	for _, c := range name {
		h = h*31 + int(c)
	}
	return h
}

func TestWeightedRandomStrategy_Select(t *testing.T) {
	s := &WeightedRandomStrategy{}
	candidates := []RouteCandidate{
		makeCandidate("a", 3, 1),
		makeCandidate("b", 1, 1),
	}

	picked, ordered := s.Select(context.Background(), candidates)
	require.NotNil(t, picked)
	assert.Len(t, ordered, 2)
	assert.Contains(t, []string{"a", "b"}, picked.Provider.Name())
}

func TestWeightedRandomStrategy_AllFallbacks(t *testing.T) {
	s := &WeightedRandomStrategy{}
	candidates := []RouteCandidate{
		makeCandidate("a", 0, 1),
		makeCandidate("b", 0, 2),
	}

	picked, ordered := s.Select(context.Background(), candidates)
	assert.Nil(t, picked)
	assert.Len(t, ordered, 2)
}

func TestSplitPrimariesFallbacks(t *testing.T) {
	candidates := []RouteCandidate{
		makeCandidate("a", 3, 1),
		makeCandidate("b", 0, 2),
		makeCandidate("c", 1, 1),
		makeCandidate("d", 0, 3),
	}

	primaries, fallbacks := splitPrimariesFallbacks(candidates)
	assert.Len(t, primaries, 2)
	assert.Len(t, fallbacks, 2)
	assert.Equal(t, "a", primaries[0].Provider.Name())
	assert.Equal(t, "b", fallbacks[0].Provider.Name())
}

func TestParseCanaryPercent(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect int
	}{
		{"valid", `{"canary_percent": 50}`, 50},
		{"zero", `{"canary_percent": 0}`, 0},
		{"negative", `{"canary_percent": -10}`, 0},
		{"over 100", `{"canary_percent": 150}`, 0},
		{"empty", "", 0},
		{"missing key", `{"other": 1}`, 0},
		{"malformed", `{invalid`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCanaryPercent([]byte(tt.input))
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestParseFallbackModels(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect []string
	}{
		{"valid", `{"fallback_models":["model-b","model-c"]}`, []string{"model-b", "model-c"}},
		{"empty", "", nil},
		{"nil", "null", nil},
		{"missing key", `{"other":1}`, nil},
		{"malformed", `{invalid`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input json.RawMessage
			if tt.input != "" {
				input = json.RawMessage(tt.input)
			}
			got := parseFallbackModels(input)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestRoundRobinStrategy_FallbackOnNilRedis(t *testing.T) {
	s := NewRoundRobinStrategy(nil)
	candidates := []RouteCandidate{
		makeCandidate("a", 1, 1),
		makeCandidate("b", 1, 1),
	}

	picked, ordered := s.Select(context.Background(), candidates)
	require.NotNil(t, picked)
	assert.Contains(t, []string{"a", "b"}, picked.Provider.Name())
	assert.Len(t, ordered, 2)
}

func TestRoundRobinStrategy_AllFallbacks(t *testing.T) {
	s := NewRoundRobinStrategy(nil)
	candidates := []RouteCandidate{
		makeCandidate("a", 0, 1),
	}

	picked, ordered := s.Select(context.Background(), candidates)
	assert.Nil(t, picked)
	assert.Len(t, ordered, 1)
}

func TestParseExtraConfig(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectCanary  int
		expectRetries int
		expectFB      []string
	}{
		{"all fields", `{"canary_percent":50,"num_retries":3,"fallback_models":["b"]}`, 50, 3, []string{"b"}},
		{"empty", "", 0, 0, nil},
		{"invalid canary negative", `{"canary_percent":-10}`, 0, 0, nil},
		{"invalid canary over 100", `{"canary_percent":150}`, 0, 0, nil},
		{"malformed", `{invalid`, 0, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input json.RawMessage
			if tt.input != "" {
				input = json.RawMessage(tt.input)
			}
			got := parseExtraConfig(input)
			assert.Equal(t, tt.expectCanary, got.CanaryPercent)
			assert.Equal(t, tt.expectRetries, got.NumRetries)
			assert.Equal(t, tt.expectFB, got.FallbackModels)
		})
	}
}

func intPtrRouter(v int) *int { return &v }

func TestRouteResultCarriesMaxContext(t *testing.T) {
	c := RouteCandidate{ProviderModel: "gpt-4o", MaxContext: intPtrRouter(128000)}
	rr := candidateToRouteResult(c)
	if rr.MaxContext == nil || *rr.MaxContext != 128000 {
		t.Fatalf("MaxContext not carried: %+v", rr.MaxContext)
	}
	c2 := RouteCandidate{ProviderModel: "m"}
	rr2 := candidateToRouteResult(c2)
	if rr2.MaxContext != nil {
		t.Fatalf("nil MaxContext must stay nil, got %v", *rr2.MaxContext)
	}
}
