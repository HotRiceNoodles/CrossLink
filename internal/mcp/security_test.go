package mcp

import (
	"testing"
	"time"
)

func TestValidateServerURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid https", "https://8.8.8.8/mcp", false},
		{"valid http", "http://1.1.1.1/mcp", false},
		{"valid with port", "https://8.8.8.8:8080/mcp", false},
		{"loopback IP", "http://127.0.0.1/mcp", true},
		{"loopback localhost", "http://localhost/mcp", true}, // resolves to 127.0.0.1 -> restricted
		{"private class A", "http://10.0.0.1/mcp", true},
		{"private class B", "http://172.16.0.1/mcp", true},
		{"private class C", "http://192.168.1.1/mcp", true},
		{"link-local", "http://169.254.1.1/mcp", true},
		{"cloud metadata IP", "http://169.254.169.254/latest/meta-data/", true},
		{"cloud metadata domain", "http://metadata.internal/latest/", true},
		{"ftp scheme", "ftp://example.com/mcp", true},
		{"no scheme", "example.com/mcp", true},
		{"empty hostname", "http:///mcp", true},
		{"invalid URL", "://bad", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServerURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateServerURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestSanitizeServer(t *testing.T) {
	srv := &MCPServer{
		Name:          "test",
		AuthConfig:    []byte(`{"token":"secret"}`),
		CustomHeaders: []byte(`{"X-Custom":"val"}`),
	}
	sanitizeServer(srv)
	if srv.AuthConfig != nil {
		t.Error("AuthConfig should be nil after sanitization")
	}
	if srv.CustomHeaders != nil {
		t.Error("CustomHeaders should be nil after sanitization")
	}
	if srv.Name != "test" {
		t.Error("Name should be preserved")
	}
}

func TestValidateSameOrigin(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		target   string
		wantErr  bool
	}{
		{"same origin", "http://example.com/sse", "http://example.com/message", false},
		{"same host different port", "http://example.com:8080/sse", "http://example.com:9090/message", true},
		{"different host", "http://example.com/sse", "http://evil.com/message", true},
		{"different scheme", "https://example.com/sse", "http://example.com/message", true},
		{"invalid target URL", "http://example.com/sse", "://bad", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSameOrigin(tt.baseURL, tt.target)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSameOrigin(%q, %q) error = %v, wantErr %v", tt.baseURL, tt.target, err, tt.wantErr)
			}
		})
	}
}

func TestRateLimiter_Eviction(t *testing.T) {
	rl := NewRateLimiter(100)
	// Create some stale buckets by manipulating lastFill directly
	stale := bucketTTL * -2
	rl.mu.Lock()
	for i := 0; i < 50; i++ {
		rl.buckets[string(rune(i))] = &tokenBucket{
			tokens:   1,
			lastFill: time.Now().Add(stale),
		}
	}
	rl.mu.Unlock()

	// Trigger eviction by filling past maxBuckets
	for i := 50; i < maxBuckets+10; i++ {
		rl.Allow(string(rune(i)))
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()
	for i := 0; i < 50; i++ {
		if _, ok := rl.buckets[string(rune(i))]; ok {
			t.Errorf("stale bucket %d should have been evicted", i)
		}
	}
}

func TestRateLimiter_Allow(t *testing.T) {
	rl := NewRateLimiter(2)
	if !rl.Allow("key1") {
		t.Error("first request should be allowed")
	}
	if !rl.Allow("key1") {
		t.Error("second request should be allowed")
	}
	if rl.Allow("key1") {
		t.Error("third request should be rejected (bucket empty)")
	}
}
