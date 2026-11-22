package service

import (
	"context"
	"testing"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M0 audit-trail tests: version-rule strips flow from the adapter checkpoint
// through the per-attempt context sink into the usage-log attempts timeline —
// including on an otherwise CLEAN single attempt (the common default-value
// strip), which the timeline used to suppress entirely.

func TestGatewayService_Chat_RecordsParamStripsOnAttempt(t *testing.T) {
	t.Parallel()
	srv, cr, _ := anthropicUpstream(t, 200)
	reg := provider.NewRegistry()
	anth, err := provider.NewAnthropicProvider("an26", srv.URL, "k", []byte(`{"api_version":"2026-10-21"}`), 5000000000)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	reg.Register("an26", anth)
	resolver := matrixResolver(reg, model.ProviderModel{
		ID: 71, ModelName: "m7", ProviderModel: "an-model", Weight: 1, Status: 1,
		Provider: model.Provider{Name: "an26", Status: 1, ID: 71},
	})
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	res, err := svc.Chat(context.Background(), &domain.AnthropicRequest{
		Model: "m7", MaxTokens: 32, Temperature: f64(1.0), // default value → silent strip
		Messages: makeMessages("hi"),
	}, "", 0)
	require.NoError(t, err)
	require.NotEmpty(t, res.Attempts, "clean single attempt must still be recorded when strips happened")
	require.NotEmpty(t, res.Attempts[0].ParamStrips, "the default-value strip must ride the attempt audit record")

	strip := res.Attempts[0].ParamStrips[0]
	assert.Equal(t, "an26", strip.Provider)
	assert.Equal(t, "2026-10-21", strip.APIVersion)
	assert.Equal(t, "temperature", strip.Param)
	assert.Equal(t, "default", strip.Reason)

	// And the wire stays clean (the strip actually happened).
	if _, has := cr.last()["temperature"]; has {
		t.Fatal("temperature must not be on the wire")
	}
}

func TestUpstreamStripSink_NoSinkIsNoOp(t *testing.T) {
	t.Parallel()
	// Direct adapter calls (probes, tests) carry no sink — recording must
	// never panic or fail the request.
	upstream.RecordParamStrip(context.Background(), upstream.ParamStrip{Param: "x"})
	var sink []upstream.ParamStrip
	ctx := upstream.WithStripSink(context.Background(), &sink)
	upstream.RecordParamStrip(ctx, upstream.ParamStrip{Param: "temperature", Reason: "default"})
	require.Len(t, sink, 1)
}

func TestAttachStripsToAttempts_SkippedAlignment(t *testing.T) {
	t.Parallel()
	attempts := []FallbackAttempt{
		{ProviderName: "a", Skipped: true},
		{ProviderName: "b"},
		{ProviderName: "c"},
	}
	batches := [][]upstream.ParamStrip{
		{{Provider: "b", Param: "top_p", Reason: "compat_strip"}}, // batch 0 → attempt "b" (skipped "a" never ran a closure)
		{}, // batch 1 → attempt "c" (no strips)
	}
	AttachStripsToAttempts(attempts, batches)
	require.NotEmpty(t, attempts[1].ParamStrips)
	assert.Equal(t, "b", attempts[1].ParamStrips[0].Provider)
	assert.Empty(t, attempts[2].ParamStrips)
}
