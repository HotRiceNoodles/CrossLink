package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

var blockedHeaders = map[string]bool{
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
	"host":              true,
	"upgrade":           true,
	// Auth headers are owned by the configured Authenticator (auth.Apply). Allowing
	// them as custom headers would let a server's customHeaders override/strip the
	// intended upstream authentication.
	"authorization":  true,
	"proxy-authorization": true,
	"cookie":         true,
}

func isBlockedHeader(name string) bool {
	return blockedHeaders[strings.ToLower(name)]
}

const maxResponseBodySize = 10 << 20 // 10MB

type HTTPTransport struct {
	serverURL     string
	httpClient    *http.Client
	auth          Authenticator
	customHeaders map[string]string
}

func NewHTTPTransport(serverURL string, auth Authenticator, customHeaders map[string]string, sharedTransport *http.Transport) *HTTPTransport {
	transport := sharedTransport
	if transport == nil {
		transport = newFallbackMCPTransport()
	}
	return &HTTPTransport{
		serverURL:     serverURL,
		httpClient:    mcpHTTPClient(transport, 30*time.Second),
		auth:          auth,
		customHeaders: customHeaders,
	}
}

func (t *HTTPTransport) Send(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.serverURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")

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

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		return nil, fmt.Errorf("MCP server returned %d: %s", resp.StatusCode, string(respBody))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return t.readSSEResponse(resp.Body, req.ID)
	}

	// Direct JSON response
	var rpcResp JSONRPCResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&rpcResp); err != nil {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		if len(respBody) > 0 {
			return nil, fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
		}
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &rpcResp, nil
}

func (t *HTTPTransport) readSSEResponse(body io.Reader, reqID json.RawMessage) (*JSONRPCResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		slog.Debug("SSE line", "line", line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "" || data == "[DONE]" {
			continue
		}
		var rpcResp JSONRPCResponse
		if err := json.Unmarshal([]byte(data), &rpcResp); err != nil {
			slog.Debug("SSE data unmarshal failed", "data", data, "error", err)
			continue
		}
		if reqID == nil || rpcResp.ID != nil {
			return &rpcResp, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}
	return nil, fmt.Errorf("no JSON-RPC response received from SSE stream")
}

func (t *HTTPTransport) Ping(ctx context.Context) error {
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
	// Fallback: check basic HTTP connectivity
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodHead, t.serverURL, nil)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	httpReq.Header.Set("Accept", "*/*")
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

func (t *HTTPTransport) Close() error { return nil }
