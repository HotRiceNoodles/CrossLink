package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

type mockProvider struct {
	name string
}

func (m *mockProvider) Chat(_ context.Context, _ *domain.OpenAIRequest, _ string) (*domain.OpenAIResponse, error) {
	return &domain.OpenAIResponse{
		Usage:   domain.OpenAIUsage{PromptTokens: 10, CompletionTokens: 20},
		Choices: []domain.OpenAIChoice{{Message: domain.OpenAIMessage{Role: "assistant", Content: "hello"}}},
	}, nil
}

func (m *mockProvider) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	ch := make(chan domain.SSEChunk, 1)
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	return ch, nil
}

func (m *mockProvider) Name() string { return m.name }

type mockRepo struct {
	data map[string][]model.ProviderModel
}

func (m *mockRepo) FindByModelName(_ context.Context, name string, _ int64) ([]model.ProviderModel, error) {
	return m.data[name], nil
}

func makeMessages(text string) []domain.AnthropicMessage {
	content, _ := json.Marshal([]domain.ContentBlock{{Type: "text", Text: text}})
	return []domain.AnthropicMessage{{Role: "user", Content: content}}
}

func TestGatewayService_Chat(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("test", &mockProvider{name: "test"})

	repo := &mockRepo{
		data: map[string][]model.ProviderModel{
			"claude-3": {{
				ID: 1, ProviderModel: "test-model", Weight: 1, Status: 1,
				InputPrice: 0.01, OutputPrice: 0.02,
				Provider:   model.Provider{Name: "test", Status: 1, ID: 1},
			}},
		},
	}

	resolver := router.NewResolver(reg, repo, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil)
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	req := &domain.AnthropicRequest{
		Model:     "claude-3",
		MaxTokens: 100,
		Messages:  makeMessages("hi"),
	}

	result, err := svc.Chat(context.Background(), req, "", 0)
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if result.InputTokens != 10 {
		t.Errorf("input tokens = %d, want 10", result.InputTokens)
	}
	if result.OutputTokens != 20 {
		t.Errorf("output tokens = %d, want 20", result.OutputTokens)
	}
	if result.ProviderName != "test" {
		t.Errorf("provider = %q, want test", result.ProviderName)
	}
	if result.ModelUsed != "test-model" {
		t.Errorf("model used = %q, want test-model", result.ModelUsed)
	}
}

func TestGatewayService_ChatNoProvider(t *testing.T) {
	reg := provider.NewRegistry()
	repo := &mockRepo{data: map[string][]model.ProviderModel{}}
	resolver := router.NewResolver(reg, repo, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil)
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	req := &domain.AnthropicRequest{
		Model:     "nonexistent",
		MaxTokens: 100,
		Messages:  makeMessages("hi"),
	}

	_, err := svc.Chat(context.Background(), req, "", 0)
	if err == nil {
		t.Error("expected error for nonexistent model")
	}
}

func TestGatewayService_StreamChat(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("test", &mockProvider{name: "test"})

	repo := &mockRepo{
		data: map[string][]model.ProviderModel{
			"claude-3": {{
				ID: 1, ProviderModel: "test-model", Weight: 1, Status: 1,
				InputPrice: 0.01, OutputPrice: 0.02,
				Provider:   model.Provider{Name: "test", Status: 1, ID: 1},
			}},
		},
	}

	resolver := router.NewResolver(reg, repo, nil, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil)
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	req := &domain.AnthropicRequest{
		Model:     "claude-3",
		MaxTokens: 100,
		Messages:  makeMessages("hi"),
	}

	var events []StreamEvent
	result, err := svc.StreamChat(context.Background(), req, func(_ context.Context, event StreamEvent) bool {
		events = append(events, event)
		return true
	}, "", 0)

	if err != nil {
		t.Fatalf("StreamChat() error: %v", err)
	}
	if result.ProviderName != "test" {
		t.Errorf("provider = %q, want test", result.ProviderName)
	}
	if len(events) == 0 {
		t.Error("expected at least one stream event")
	}
}
