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
			if sErr := r.store.Save(ctx, &Fixture{
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
		for chunk := range ch {
			out <- chunk
			recorded = append(recorded, chunk)
		}
		// Save fixture after stream ends. Use background context (request ctx
		// may be cancelled by the time we get here).
		store := r.activeStore()
		if store != nil && len(recorded) > 0 {
			chunksJSON, mErr := json.Marshal(recorded)
			if mErr != nil {
				slog.Warn("recording: marshal stream chunks failed", "provider", r.name, "error", mErr)
				return
			}
			if sErr := r.store.Save(context.Background(), &Fixture{
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
