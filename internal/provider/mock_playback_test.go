package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMockProvider_PlaybackChat_HitFixture(t *testing.T) {
	store := newMockFixtureStore()
	// Pre-populate a fixture.
	req := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hello"}}}
	recorded := &domain.OpenAIResponse{ID: "recorded-1", Model: "m", Object: "chat.completion",
		Choices: []domain.OpenAIChoice{{Index: 0, Message: domain.OpenAIMessage{Role: "assistant", Content: "real AI reply"}, FinishReason: "stop"}}}
	body, _ := json.Marshal(recorded)
	store.Save(context.Background(), &Fixture{Model: "m", RequestHash: RequestHash(req), ResponseBody: body, IsStream: false})

	p := &MockProvider{name: "mock", fixtures: store}
	got, err := p.Chat(context.Background(), req, "")
	require.NoError(t, err)
	assert.Equal(t, "recorded-1", got.ID, "should return recorded response, not canned")
	assert.Equal(t, "real AI reply", got.Choices[0].Message.Content)
}

func TestMockProvider_PlaybackChat_MissFallback(t *testing.T) {
	store := newMockFixtureStore() // empty — no fixtures
	p := &MockProvider{name: "mock", fixtures: store}
	req := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	got, err := p.Chat(context.Background(), req, "")
	require.NoError(t, err)
	assert.Contains(t, got.Choices[0].Message.Content, "[Mock]", "should return canned when no fixture")
}

func TestMockProvider_PlaybackStream_HitFixture(t *testing.T) {
	store := newMockFixtureStore()
	// Record a 2-chunk stream.
	c1 := domain.SSEChunk{Chunk: &domain.OpenAIChunk{}}
	c1.Chunk.Choices = []domain.OpenAIChunkChoice{{Delta: domain.OpenAIChunkDelta{Content: "streamed"}}}
	chunks := []domain.SSEChunk{c1, {Done: true}}
	chunksJSON, _ := json.Marshal(chunks)
	req := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "x"}}}
	store.Save(context.Background(), &Fixture{Model: "m", RequestHash: RequestHash(req), StreamChunks: chunksJSON, IsStream: true})

	p := &MockProvider{name: "mock", fixtures: store}
	out, err := p.StreamChat(context.Background(), req, "")
	require.NoError(t, err)
	var received []domain.SSEChunk
	for c := range out {
		received = append(received, c)
	}
	assert.Len(t, received, 2, "should replay recorded chunks")
	assert.True(t, received[1].Done)
}
