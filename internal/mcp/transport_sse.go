package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type SSETransport struct {
	serverURL     string
	httpClient    *http.Client
	auth          Authenticator
	customHeaders map[string]string
	mu            sync.Mutex
	messageURL    string
}

func NewSSETransport(serverURL string, auth Authenticator, customHeaders map[string]string, sharedTransport *http.Transport) *SSETransport {
	transport := sharedTransport
	if transport == nil {
		transport = newFallbackMCPTransport()
	}
	return &SSETransport{
		serverURL:     serverURL,
		httpClient:    mcpHTTPClient(transport, 30*time.Second),
		auth:          auth,
		customHeaders: customHeaders,
	}
}

func (t *SSETransport) discoverEndpoint(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.messageURL != "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.serverURL, nil)
	if err != nil {
		return fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if t.auth != nil {
		if err := t.auth.Apply(req); err != nil {
			return fmt.Errorf("apply auth: %w", err)
		}
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect SSE: %w", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			discovered := strings.TrimPrefix(line, "data: ")
			if err := validateSameOrigin(t.serverURL, discovered); err != nil {
				return fmt.Errorf("SSE endpoint validation: %w", err)
			}
			t.messageURL = discovered
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SSE stream: %w", err)
	}
	return fmt.Errorf("SSE endpoint discovery failed: no endpoint event received")
}

func (t *SSETransport) Send(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if err := t.discoverEndpoint(ctx); err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	t.mu.Lock()
	msgURL := t.messageURL
	t.mu.Unlock()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, msgURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if t.auth != nil {
		if err := t.auth.Apply(httpReq); err != nil {
			return nil, fmt.Errorf("apply auth: %w", err)
		}
	}
	for k, v := range t.customHeaders {
		if isBlockedHeader(k) {
			continue
		}
		httpReq.Header.Set(k, v)
	}

	resp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("MCP server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var rpcResp JSONRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &rpcResp, nil
}

func (t *SSETransport) Ping(ctx context.Context) error {
	// Try MCP ping first
	req := &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"ping"`),
		Method:  MethodPing,
	}
	resp, err := t.Send(ctx, req)
	if err == nil && resp.Error == nil {
		return nil
	}
	// Fallback: check basic HTTP connectivity to SSE endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, t.serverURL, nil)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	if t.auth != nil {
		_ = t.auth.Apply(httpReq)
	}
	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("server unreachable: %w", err)
	}
	httpResp.Body.Close()
	if httpResp.StatusCode >= 500 {
		return fmt.Errorf("server returned %d", httpResp.StatusCode)
	}
	return nil
}

func (t *SSETransport) Close() error {
	t.mu.Lock()
	t.messageURL = ""
	t.mu.Unlock()
	return nil
}

func validateSameOrigin(baseURL, discoveredURL string) error {
	base, err := url.Parse(baseURL)
	if err != nil {
		return err
	}
	target, err := url.Parse(discoveredURL)
	if err != nil {
		return fmt.Errorf("invalid message URL: %w", err)
	}
	if target.Scheme != base.Scheme || target.Hostname() != base.Hostname() {
		return fmt.Errorf("SSE endpoint redirected to different origin: %s", discoveredURL)
	}
	return nil
}
