package provider

import (
	"context"
	"io"
	"net/url"

	"github.com/crosslink/internal/domain"
)

type Provider interface {
	Chat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (*domain.OpenAIResponse, error)
	StreamChat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (<-chan domain.SSEChunk, error)
	Name() string
}

type EmbeddingsProvider interface {
	Provider
	Embeddings(ctx context.Context, req *domain.EmbeddingsRequest, apiKey string) (*domain.EmbeddingsResponse, error)
}

type ImageProvider interface {
	Provider
	GenerateImage(ctx context.Context, req *domain.ImageRequest, apiKey string) (*domain.ImageResponse, error)
}

type VideoProvider interface {
	Provider
	SubmitVideoTask(ctx context.Context, req *domain.VideoRequest, apiKey string) (*domain.VideoTask, error)
	GetVideoTaskStatus(ctx context.Context, taskID string, apiKey string) (*domain.VideoTask, error)
}

type AudioProvider interface {
	Provider
	CreateSpeech(ctx context.Context, req *domain.SpeechRequest, apiKey string) (io.ReadCloser, string, error)
	Transcribe(ctx context.Context, req *domain.TranscriptionRequest, apiKey string) (*domain.TranscriptionResponse, error)
	Translate(ctx context.Context, req *domain.TranscriptionRequest, apiKey string) (*domain.TranscriptionResponse, error)
}

type BatchProvider interface {
	Provider
	CreateBatch(ctx context.Context, req *domain.BatchRequest, apiKey string) (*domain.BatchResponse, error)
	GetBatch(ctx context.Context, batchID string, apiKey string) (*domain.BatchResponse, error)
	ListBatches(ctx context.Context, params url.Values, apiKey string) (*domain.BatchListResponse, error)
	CancelBatch(ctx context.Context, batchID string, apiKey string) (*domain.BatchResponse, error)
}

// ResponsesProvider serves the OpenAI Responses API (/v1/responses) via raw
// upstream passthrough (3A). Responses returns the upstream's raw HTTP body
// (non-streaming JSON or streaming SSE bytes) and its status code; the handler
// copies bytes through without deserializing, so no unknown fields are lost.
// A provider declares support via the model-level ExtraConfig flag
// "supports_responses": true (the interface alone does not imply upstream
// support — openai_compatible is a generic adapter).
type ResponsesProvider interface {
	Provider
	Responses(ctx context.Context, rawBody []byte, apiKey string) (io.ReadCloser, int, error)
}
