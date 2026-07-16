package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
)

type AzureOpenAIProvider struct {
	name         string
	endpoint     string
	deployName   string
	apiVersion   string
	httpClient   *http.Client
	streamClient *http.Client
}

type azureExtraConfig struct {
	DeploymentName string `json:"deployment_name"`
	APIVersion     string `json:"api_version"`
}

func NewAzureOpenAIProvider(name, baseURL, apiKey string, extraConfig []byte, timeout time.Duration) (*AzureOpenAIProvider, error) {
	p := &AzureOpenAIProvider{
		name:       name,
		endpoint:   strings.TrimRight(baseURL, "/"),
		apiVersion: "2024-02-15-preview",
		httpClient:   newDefaultClient(timeout),
		streamClient: newStreamClient(),
	}

	if len(extraConfig) > 0 {
		var cfg azureExtraConfig
		if err := json.Unmarshal(extraConfig, &cfg); err == nil {
			if cfg.DeploymentName != "" {
				p.deployName = cfg.DeploymentName
			}
			if cfg.APIVersion != "" {
				p.apiVersion = cfg.APIVersion
			}
		}
	}

	if p.deployName == "" {
		return nil, fmt.Errorf("azure_openai: deployment_name is required in extra_config")
	}

	return p, nil
}

func (p *AzureOpenAIProvider) Name() string { return p.name }

func (p *AzureOpenAIProvider) buildURL() string {
	return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
		p.endpoint, p.deployName, p.apiVersion)
}

func (p *AzureOpenAIProvider) embeddingsURL() string {
	return fmt.Sprintf("%s/openai/deployments/%s/embeddings?api-version=%s",
		p.endpoint, p.deployName, p.apiVersion)
}

func (p *AzureOpenAIProvider) Embeddings(ctx context.Context, req *domain.EmbeddingsRequest, apiKey string) (*domain.EmbeddingsResponse, error) {
	key := apiKey
	if key == "" {
		return nil, fmt.Errorf("azure_openai: api_key is required")
	}

	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.embeddingsURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("api-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}

	var embeddingsResp domain.EmbeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&embeddingsResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &embeddingsResp, nil
}

func (p *AzureOpenAIProvider) Chat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (*domain.OpenAIResponse, error) {
	key := apiKey
	if key == "" {
		return nil, fmt.Errorf("azure_openai: api_key is required")
	}

	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.buildURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("api-key", key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}

	var openaiResp domain.OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&openaiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &openaiResp, nil
}

func (p *AzureOpenAIProvider) StreamChat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (<-chan domain.SSEChunk, error) {
	key := apiKey
	if key == "" {
		return nil, fmt.Errorf("azure_openai: api_key is required")
	}

	reqCopy := *req
	reqCopy.Stream = true
	body, _ := json.Marshal(reqCopy)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.buildURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("api-key", key)

	resp, err := p.streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		err := parseProviderError(resp)
		resp.Body.Close()
		return nil, err
	}

	ch := make(chan domain.SSEChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		readSSEStream(resp.Body, ch)
	}()

	return ch, nil
}

func init() {
	RegisterAdapter("azure_openai", func(p *model.Provider, timeout time.Duration) (Provider, error) {
		return NewAzureOpenAIProvider(p.Name, p.BaseURL, p.APIKey, p.ExtraConfig, timeout)
	}, &AdapterMeta{
		DisplayName:  "Azure OpenAI",
		Description:  "Microsoft Azure OpenAI Service",
		NeedsBaseURL: true,
		NeedsAPIKey:  true,
		Capabilities: []string{"chat", "stream", "embeddings"},
		ExtraFields: []AdapterField{
			{Name: "deployment_name", Type: "text", Required: true, Label: "Deployment Name", Placeholder: "my-gpt4-deployment"},
			{Name: "api_version", Type: "text", Label: "API Version", DefaultValue: "2024-02-15-preview"},
			{Name: "record", Label: "录制响应（Mock 回放）", Type: "switch", DefaultValue: "false"},
		},
	})
}
