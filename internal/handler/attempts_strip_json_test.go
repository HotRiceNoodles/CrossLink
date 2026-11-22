package handler

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M0 audit: attemptsJSON must NOT suppress a clean single attempt that
// performed param strips — the strip audit row is the whole point.
func TestAttemptsJSON_SingleSuccessWithStripsNotSuppressed(t *testing.T) {
	t.Parallel()

	// Baseline: clean single success → nil (row stays flat for the common case).
	assert.Nil(t, attemptsJSON([]service.FallbackAttempt{
		{ProviderName: "p", ProviderModel: "m", Success: true},
	}))

	// With a strip → timeline survives and carries the audit record.
	out := attemptsJSON([]service.FallbackAttempt{
		{ProviderName: "p", ProviderModel: "m", Success: true, ParamStrips: []upstream.ParamStrip{
			{Provider: "p", APIVersion: "2026-10-21", Param: "temperature", Reason: "default"},
		}},
	})
	require.NotNil(t, out)

	var records []map[string]any
	require.NoError(t, json.Unmarshal(out, &records))
	require.Len(t, records, 1)
	strips, ok := records[0]["param_strips"].([]any)
	require.True(t, ok, "param_strips must serialize: %v", records[0])
	require.Len(t, strips, 1)
	strip := strips[0].(map[string]any)
	assert.Equal(t, "temperature", strip["param"])
	assert.Equal(t, "default", strip["reason"])
	assert.Equal(t, "2026-10-21", strip["api_version"])
}
