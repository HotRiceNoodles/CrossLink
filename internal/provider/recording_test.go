package provider

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockFixtureStore is an in-memory FixtureStore for testing.
type mockFixtureStore struct {
	fixtures map[string]*Fixture // key: model+":"+hash
	saved    int
}

func newMockFixtureStore() *mockFixtureStore {
	return &mockFixtureStore{fixtures: make(map[string]*Fixture)}
}

func (s *mockFixtureStore) Save(_ context.Context, f *Fixture) error {
	s.fixtures[f.Model+":"+f.RequestHash] = f
	s.saved++
	return nil
}

func (s *mockFixtureStore) Lookup(_ context.Context, model, hash string) (*Fixture, bool, error) {
	f, ok := s.fixtures[model+":"+hash]
	return f, ok, nil
}

// fakeProvider is a minimal Provider for testing RecordingProvider.
type fakeProvider struct {
	resp  *domain.OpenAIResponse
	ch    chan domain.SSEChunk
	err   error
	name  string
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Chat(_ context.Context, _ *domain.OpenAIRequest, _ string) (*domain.OpenAIResponse, error) {
	return f.resp, f.err
}
func (f *fakeProvider) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ch, nil
}

func TestRecordingProvider_Chat_SavesFixture(t *testing.T) {
	store := newMockFixtureStore()
	resp := &domain.OpenAIResponse{ID: "test-1", Model: "gpt-4o", Object: "chat.completion"}
	inner := &fakeProvider{resp: resp, name: "real-openai"}
	rp := NewRecordingProvider(inner, "real-openai", store)

	req := &domain.OpenAIRequest{Model: "gpt-4o", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	got, err := rp.Chat(context.Background(), req, "key")
	require.NoError(t, err)
	assert.Equal(t, "test-1", got.ID)
	assert.Equal(t, 1, store.saved, "fixture should be saved")

	// Verify fixture content.
	hash := RequestHash(req)
	f, ok, _ := store.Lookup(context.Background(), "gpt-4o", hash)
	require.True(t, ok)
	assert.False(t, f.IsStream)
	var stored domain.OpenAIResponse
	require.NoError(t, json.Unmarshal(f.ResponseBody, &stored))
	assert.Equal(t, "test-1", stored.ID)
}

func TestRecordingProvider_Chat_ErrorNotRecorded(t *testing.T) {
	store := newMockFixtureStore()
	inner := &fakeProvider{err: assertError("upstream down")}
	rp := NewRecordingProvider(inner, "p", store)

	req := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "x"}}}
	_, err := rp.Chat(context.Background(), req, "")
	require.Error(t, err)
	assert.Equal(t, 0, store.saved, "errors must not be recorded")
}

func TestRecordingProvider_StreamChat_SavesChunks(t *testing.T) {
	store := newMockFixtureStore()
	ch := make(chan domain.SSEChunk, 4)
	ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Delta: domain.OpenAIChunkDelta{Content: "hel"}}}}}
	ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Delta: domain.OpenAIChunkDelta{Content: "lo"}}}}}
	ch <- domain.SSEChunk{Done: true}
	close(ch)

	inner := &fakeProvider{ch: ch, name: "real"}
	rp := NewRecordingProvider(inner, "real", store)

	req := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	out, err := rp.StreamChat(context.Background(), req, "")
	require.NoError(t, err)

	// Drain output — chunks forwarded immediately.
	var received []domain.SSEChunk
	for chunk := range out {
		received = append(received, chunk)
	}
	assert.Len(t, received, 3, "all chunks forwarded")
	// Give the save goroutine a moment.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 1, store.saved, "stream fixture saved after drain")

	hash := RequestHash(req)
	f, ok, _ := store.Lookup(context.Background(), "m", hash)
	require.True(t, ok)
	assert.True(t, f.IsStream)
}

func TestRecordingProvider_StreamChat_ForwardsWithoutDelay(t *testing.T) {
	store := newMockFixtureStore()
	ch := make(chan domain.SSEChunk, 2)
	ch <- domain.SSEChunk{Chunk: &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Delta: domain.OpenAIChunkDelta{Content: "fast"}}}}}
	ch <- domain.SSEChunk{Done: true}
	close(ch)

	inner := &fakeProvider{ch: ch, name: "p"}
	rp := NewRecordingProvider(inner, "p", store)

	start := time.Now()
	out, _ := rp.StreamChat(context.Background(), &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "x"}}}, "")
	for range out {
	}
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 100*time.Millisecond, "tee must not add significant delay")
}

func TestRequestHash_Deterministic(t *testing.T) {
	req := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{
		{Role: "user", Content: "hello"},
	}}
	h1 := RequestHash(req)
	h2 := RequestHash(req)
	assert.Equal(t, h1, h2, "same request must produce same hash")
	assert.Len(t, h1, 64, "SHA256 hex = 64 chars")
}

func TestRequestHash_IgnoresTemperature(t *testing.T) {
	req1 := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}, Temperature: floatPtr(0.7)}
	req2 := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}, Temperature: floatPtr(1.0)}
	assert.Equal(t, RequestHash(req1), RequestHash(req2), "same prompt, different temp → same hash")
}

func TestRequestHash_DifferentPrompts(t *testing.T) {
	req1 := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	req2 := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "bye"}}}
	assert.NotEqual(t, RequestHash(req1), RequestHash(req2), "different prompts → different hash")
}

func TestIsRecordEnabled(t *testing.T) {
	assert.False(t, IsRecordEnabled(nil))
	assert.False(t, IsRecordEnabled([]byte(`{}`)))
	assert.False(t, IsRecordEnabled([]byte(`{"record":false}`)))
	assert.True(t, IsRecordEnabled([]byte(`{"record":true}`)))
	assert.True(t, IsRecordEnabled([]byte(`{"record":true,"api_protocol":"openai"}`)))
	assert.False(t, IsRecordEnabled([]byte(`not json`)))
}

// helpers
func assertError(msg string) error { return &simpleErr{msg: msg} }

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

func floatPtr(v float64) *float64 { return &v }
