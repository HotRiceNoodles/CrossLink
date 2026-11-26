package service

import (
	"context"
	"testing"
)

// Phase-1 replay binding (2026-11-25 checklist §6.2): the shared read entry
// enforces endpoint + fingerprint; bound entries re-authorize their original
// execution target at the handler layer (see anthropic handler).

func TestIdempotency_GetVerified_BindingConflicts(t *testing.T) {
	t.Parallel()
	c := NewIdempotencyCache(newIdemTestRedis(t))
	ctx := context.Background()

	if err := c.SetWithBinding(ctx, 7, "k", &CachedResponse{StatusCode: 200, Body: []byte(`{}`)}, IdemBinding{
		Endpoint: "/v1/messages", Fingerprint: "fp-1", ExecProvider: "pA", ExecModel: "mA",
	}); err != nil {
		t.Fatal(err)
	}

	// Same endpoint + fingerprint: ok, binding fields round-trip.
	resp, verdict, reason := c.GetVerified(ctx, 7, "k", "/v1/messages", "fp-1")
	if verdict != IdemOK || reason != "" || resp == nil {
		t.Fatalf("matching read = %v/%q", verdict, reason)
	}
	if resp.ExecProvider != "pA" || resp.ExecModel != "mA" {
		t.Fatalf("binding lost: %+v", resp)
	}

	// Cross-endpoint replay (shared key space): definite conflict.
	if _, verdict, reason := c.GetVerified(ctx, 7, "k", "/v1/chat/completions", "fp-1"); verdict != IdemConflict || reason == "" {
		t.Fatalf("cross-endpoint = %v/%q, want conflict with reason", verdict, reason)
	}

	// Same endpoint, different body: definite conflict.
	if _, verdict, _ := c.GetVerified(ctx, 7, "k", "/v1/messages", "fp-2"); verdict != IdemConflict {
		t.Fatal("same key different body must conflict")
	}

	// Different API key: independent namespace (miss, not conflict).
	if _, verdict, _ := c.GetVerified(ctx, 8, "k", "/v1/messages", "fp-1"); verdict != IdemMiss {
		t.Fatal("other key must be a miss")
	}

	// Legacy entry (plain Set, no binding): reads OK — callers apply the
	// conservative full re-check.
	if err := c.Set(ctx, 7, "legacy", &CachedResponse{StatusCode: 200, Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, verdict, _ := c.GetVerified(ctx, 7, "legacy", "/v1/messages", "anything"); verdict != IdemOK {
		t.Fatal("legacy entry without binding must read OK")
	}
}
