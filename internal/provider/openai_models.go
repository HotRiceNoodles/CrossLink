package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ListUpstreamModels enumerates models from the upstream's GET /v1/models endpoint.
// It tolerates both the OpenAI envelope {"data":[{id,owned_by}]} and a bare
// top-level array [{id,...}] returned by some compatible servers.
func (p *OpenAICompatibleProvider) ListUpstreamModels(ctx context.Context, apiKey string) ([]UpstreamModel, error) {
	url := p.baseURL + "/models"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseRead))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// Try the canonical OpenAI envelope first.
	var envelope struct {
		Data []UpstreamModel `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Data != nil {
		return envelope.Data, nil
	}

	// Fall back to a bare top-level array.
	var arr []UpstreamModel
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	// arr may be nil/empty for "{}" or "[]" bodies — that's a valid "no models" result.
	return arr, nil
}
