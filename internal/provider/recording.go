package provider

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/crosslink/internal/domain"
)

// RecordingProvider wraps a real Provider and records every successful
// (request → response) pair to a FixtureStore for later VCR-style playback by
// MockProvider. It implements the standard Provider interface, so FallbackEngine
// and the entire middleware chain are completely unaware of recording.
//
// Recording is activated per-provider via extra_config.record: true in
// syncRegistry. When recording is off, RecordingProvider is never constructed
// (zero overhead).
//
// See docs/plans/2026-07-15-mock-record-playback-design.md.
type RecordingProvider struct {
	inner Provider
	name  string
	store FixtureStore // can be nil → reads globalFixtureStore dynamically
}

func NewRecordingProvider(inner Provider, name string, store FixtureStore) *RecordingProvider {
	return &RecordingProvider{inner: inner, name: name, store: store}
}

func (r *RecordingProvider) Name() string { return r.name }

// activeStore returns the fixture store, falling back to globalFixtureStore.
// Same pattern as MockProvider.activeFixtureStore — handles startup ordering
// where RecordingProvider is constructed (RegisterProvidersFromDB /
// registry_sync) before SetGlobalFixtureStore runs.
func (r *RecordingProvider) activeStore() FixtureStore {
	if r.store != nil {
		return r.store
	}
	return globalFixtureStore
}

// Recording caps (supervisor review): an unbounded `recorded` slice grows
// with stream length and the final Marshal makes a full second copy — a
// 20MB stream would pin 40MB+ per recorded stream. Past either cap the tee
// STOPS recording (discards the partial fixture) but keeps forwarding to the
// consumer: recording must never degrade the live stream. Vars (not consts)
// ONLY so tests can lower them; production never reassigns.
var (
	recordingMaxChunks = 20000
	recordingMaxBytes  = 32 << 20 // estimated payload bytes across chunks
)

func chunkApproxBytes(c domain.SSEChunk) int {
	if c.Chunk == nil {
		return 0
	}
	n := len(c.Chunk.ID) + len(c.Chunk.Model) + 64
	for _, ch := range c.Chunk.Choices {
		n += len(ch.Delta.Role) + len(ch.Delta.Content) + len(ch.Delta.ReasoningContent) + 32
	}
	return n
}

// Chat delegates to the real provider and saves the response as a fixture.
// Errors are NOT recorded (no point replaying failures).
func (r *RecordingProvider) Chat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (*domain.OpenAIResponse, error) {
	resp, err := r.inner.Chat(ctx, req, apiKey)
	store := r.activeStore()
	if err == nil && resp != nil && store != nil {
		body, mErr := json.Marshal(resp)
		if mErr != nil {
			slog.Warn("recording: marshal response failed", "provider", r.name, "error", mErr)
		} else {
			// store (not r.store): activeStore may have resolved the global
			// fixture store while r.store is nil — calling r.store.Save here
			// panicked in exactly that case.
			if sErr := store.Save(ctx, &Fixture{
				ProviderName: r.name,
				Model:        req.Model,
				RequestHash:  RequestHash(req),
				ResponseBody: body,
				IsStream:     false,
			}); sErr != nil {
				slog.Warn("recording: save fixture failed", "provider", r.name, "error", sErr)
			}
		}
	}
	return resp, err
}

// StreamChat delegates to the real provider, teeing chunks to the caller AND
// a recorder. Chunks are forwarded immediately (no delay); the fixture is saved
// after the stream completes. Client disconnect (ctx cancel) stops recording.
func (r *RecordingProvider) StreamChat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (<-chan domain.SSEChunk, error) {
	ch, err := r.inner.StreamChat(ctx, req, apiKey)
	if err != nil {
		return nil, err
	}

	out := make(chan domain.SSEChunk, 64)
	go func() {
		defer close(out)
		var recorded []domain.SSEChunk
		var estBytes int
		overCap := false
	loop:
		for {
			// BOTH directions select on ctx: cancellation must unblock the
			// tee whether it is parked receiving from a hung inner producer
			// (a `for range ch` would wait forever for a close that never
			// comes) or sending to a gone consumer. Standard adapters close
			// their channel on ctx cancel, but the tee's own exit must not
			// depend on the inner producer's good behavior — and it never
			// drains open-endedly in place of cancellation propagation
			// (supervisor review).
			var chunk domain.SSEChunk
			select {
			case c, ok := <-ch:
				if !ok {
					break loop
				}
				chunk = c
			case <-ctx.Done():
				slog.Debug("recording: cancelled while receiving from inner stream, discarding partial fixture",
					"provider", r.name, "chunks", len(recorded))
				return
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				slog.Debug("recording: consumer gone and ctx cancelled, discarding partial fixture",
					"provider", r.name, "chunks", len(recorded))
				return
			}
			// Cap enforcement (supervisor review): stop RECORDING past the
			// chunk/byte caps — forwarding to the consumer continues
			// untouched. The partial fixture is discarded (a truncated
			// replay would be misleading) and the drop is logged once.
			if !overCap {
				recorded = append(recorded, chunk)
				estBytes += chunkApproxBytes(chunk)
				if len(recorded) > recordingMaxChunks || estBytes > recordingMaxBytes {
					overCap = true
					recorded = nil
					slog.Warn("recording: stream exceeded recording cap, fixture discarded (stream unaffected)",
						"provider", r.name, "cap_chunks", recordingMaxChunks,
						"cap_bytes", recordingMaxBytes, "est_bytes", estBytes)
				}
			}
		}
		// Save fixture after stream ends. Use background context (request ctx
		// may be cancelled by the time we get here).
		if overCap {
			return
		}
		store := r.activeStore()
		if store != nil && len(recorded) > 0 {
			chunksJSON, mErr := json.Marshal(recorded)
			if mErr != nil {
				slog.Warn("recording: marshal stream chunks failed", "provider", r.name, "error", mErr)
				return
			}
			// store (not r.store): activeStore may have resolved the global
			// fixture store while r.store is nil — r.store.Save panicked in
			// exactly that case.
			if sErr := store.Save(context.Background(), &Fixture{
				ProviderName: r.name,
				Model:        req.Model,
				RequestHash:  RequestHash(req),
				StreamChunks: chunksJSON,
				IsStream:     true,
			}); sErr != nil {
				slog.Warn("recording: save stream fixture failed", "provider", r.name, "error", sErr)
			}
		}
	}()
	return out, nil
}

// IsRecordEnabled checks extra_config for {"record": true}.
func IsRecordEnabled(extraConfig []byte) bool {
	if len(extraConfig) == 0 {
		return false
	}
	var cfg struct {
		Record bool `json:"record"`
	}
	if json.Unmarshal(extraConfig, &cfg) != nil {
		return false
	}
	return cfg.Record
}
