package provider

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/require"
)

// stubRecordingInner is a Provider stub whose stream producer mimics the
// standard adapters: it selects on ctx.Done while sending, so cancellation
// both closes its channel and exits its goroutine.
type stubRecordingInner struct {
	producerExited chan struct{}
	sendHung       bool // when true: producer blocks forever WITHOUT ctx select (contract violator)
}

func (p *stubRecordingInner) Name() string { return "stub-inner" }
func (p *stubRecordingInner) Chat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (*domain.OpenAIResponse, error) {
	return nil, nil
}

func (p *stubRecordingInner) StreamChat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (<-chan domain.SSEChunk, error) {
	ch := make(chan domain.SSEChunk, 1)
	go func() {
		defer close(ch)
		defer close(p.producerExited)
		content := "x"
		for i := 0; i < 1000; i++ {
			if p.sendHung {
				ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{
					Choices: []domain.OpenAIChunkChoice{{Index: 0, Delta: domain.OpenAIChunkDelta{Content: content}}},
				}}
				select {} // contract violator: never observes ctx
			}
			select {
			case ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{
				Choices: []domain.OpenAIChunkChoice{{Index: 0, Delta: domain.OpenAIChunkDelta{Content: content}}},
			}}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// Cancellation must exit the tee goroutine IMMEDIATELY (no open-ended drain)
// while a well-behaved inner producer also exits via ctx propagation.
func TestRecordingProvider_CancelExitsTeeAndProducer(t *testing.T) {
	inner := &stubRecordingInner{producerExited: make(chan struct{})}
	rec := NewRecordingProvider(inner, "stub", nil) // nil store: no fixture save side effects

	ctx, cancel := context.WithCancel(context.Background())
	out, err := rec.StreamChat(ctx, &domain.OpenAIRequest{}, "k")
	require.NoError(t, err)

	// Consume a little, then abandon the stream and cancel.
	<-out
	<-out
	cancel()

	select {
	case _, ok := <-out:
		// tee exits via ctx.Done and its defer close(out) fires; if !ok the
		// channel closed — good. A stray buffered chunk is also fine.
		_ = ok
	case <-time.After(3 * time.Second):
		t.Fatal("tee goroutine did not exit after cancel (draining hang?)")
	}
	// Drain anything buffered, then require close.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-out:
			if !ok {
				goto closed
			}
		case <-deadline:
			t.Fatal("out channel never closed after cancel")
		}
	}
closed:
	select {
	case <-inner.producerExited:
	case <-time.After(3 * time.Second):
		t.Fatal("inner producer did not exit on ctx propagation")
	}
}

// Even against a contract-VIOLATING inner producer (blocks forever, never
// observes ctx), the tee itself must still exit on cancel — the drain-free
// design does not tie the tee's lifetime to the inner producer's exit.
func TestRecordingProvider_CancelExitsEvenIfInnerProducerHangs(t *testing.T) {
	release := make(chan struct{})
	inner := &stubRecordingInner{producerExited: make(chan struct{}), sendHung: true}
	rec := NewRecordingProvider(inner, "stub-hang", nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		close(release)
	})
	out, err := rec.StreamChat(ctx, &domain.OpenAIRequest{}, "k")
	require.NoError(t, err)
	<-out // first chunk flows

	cancel()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-out:
			if !ok {
				return // tee exited and closed out despite the hung producer
			}
		case <-deadline:
			t.Fatal("tee stuck draining behind a hung inner producer after cancel")
		}
	}
}

// countingStore counts Save calls — proves activeStore()'s resolved store is
// the one actually used (the r.store.Save bug called the possibly-nil field).
type countingStore struct{ saves int }

func (s *countingStore) Save(ctx context.Context, f *Fixture) error { s.saves++; return nil }
func (s *countingStore) Lookup(ctx context.Context, model, hash string) (*Fixture, bool, error) {
	return nil, false, nil
}

// A RecordingProvider constructed with a nil store must Save through the
// GLOBAL fixture store resolved by activeStore — the first version called
// r.store.Save and panicked in exactly this configuration.
func TestRecording_NilStoreResolvesGlobalWithoutPanic(t *testing.T) {
	global := &countingStore{}
	old := globalFixtureStore
	globalFixtureStore = global
	t.Cleanup(func() { globalFixtureStore = old })

	rec := NewRecordingProvider(&stubRecordingInner{producerExited: make(chan struct{})}, "stub", nil)
	ch, err := rec.StreamChat(context.Background(), &domain.OpenAIRequest{Model: "m"}, "k")
	require.NoError(t, err)
	for range ch {
	}
	require.Equal(t, 1, global.saves, "stream fixture must save via the resolved global store")
}

// Past the chunk cap the tee STOPS recording (fixture discarded) while the
// consumer still receives every chunk — recording must never degrade the
// live stream.
func TestRecording_CapStopsRecordingNotStream(t *testing.T) {
	store := &countingStore{}
	oldChunks, oldBytes := recordingMaxChunks, recordingMaxBytes
	recordingMaxChunks = 100
	recordingMaxBytes = 1 << 30
	t.Cleanup(func() { recordingMaxChunks, recordingMaxBytes = oldChunks, oldBytes })

	rec := NewRecordingProvider(&stubRecordingInner{producerExited: make(chan struct{})}, "stub", store)
	ch, err := rec.StreamChat(context.Background(), &domain.OpenAIRequest{Model: "m"}, "k")
	require.NoError(t, err)
	received := 0
	for range ch {
		received++
	}
	require.Equal(t, 1000, received, "consumer must receive ALL chunks after the recording cap trips")
	require.Equal(t, 0, store.saves, "over-cap fixture must be discarded, not saved truncated")
}
