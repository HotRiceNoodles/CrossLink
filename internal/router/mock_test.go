package router

import (
	"context"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
)

type mockProvider struct{ name string }

func (m *mockProvider) Chat(_ context.Context, _ *domain.OpenAIRequest, _ string) (*domain.OpenAIResponse, error) {
	return nil, nil
}
func (m *mockProvider) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	return nil, nil
}
func (m *mockProvider) Name() string { return m.name }

var _ provider.Provider = (*mockProvider)(nil)
