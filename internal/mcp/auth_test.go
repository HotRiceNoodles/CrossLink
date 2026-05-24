package mcp

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNoAuth_Apply(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	a := &noAuth{}
	if err := a.Apply(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("expected no Authorization header, got %q", got)
	}
}

func TestBearerAuth_Apply(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	a := &bearerAuth{token: "my-token"}
	if err := a.Apply(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "Bearer my-token"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func TestBasicAuth_Apply(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	a := &basicAuth{user: "alice", pass: "secret"}
	if err := a.Apply(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req.Header.Get("Authorization")
	if !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("Authorization = %q, want Basic prefix", got)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "Basic "))
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if string(decoded) != "alice:secret" {
		t.Errorf("decoded = %q, want %q", decoded, "alice:secret")
	}
}

func TestSigv4Auth_Apply(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://example.com/path?query=val", strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("Content-Type", "application/json")

	a := &sigv4Auth{
		accessKeyID:     "AKID",
		secretAccessKey: "SECRET",
		region:          "us-east-1",
		service:         "execute-api",
	}
	if err := a.Apply(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization = %q, want AWS4-HMAC-SHA256 prefix", auth)
	}
	if !strings.Contains(auth, "Credential=AKID/") {
		t.Errorf("Authorization missing Credential with access key ID")
	}
	if req.Header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date header not set")
	}
	if req.Header.Get("X-Amz-Content-Sha256") == "" {
		t.Error("X-Amz-Content-Sha256 header not set")
	}

	// Body should still be readable after signing
	body := make([]byte, 20)
	n, _ := req.Body.Read(body)
	if n == 0 {
		t.Error("body is empty after signing — rewind failed")
	}
}

func TestOAuth2Auth_TokenFetchAndCache(t *testing.T) {
	var tokenCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Basic ") {
			t.Errorf("token request missing Basic auth, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "test-access-token",
			"expires_in":   3600,
		})
	}))
	defer ts.Close()

	a := &oauth2Auth{
		clientID:     "my-client",
		clientSecret: "my-secret",
		tokenURL:     ts.URL,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
	}

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	if err := a.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := req.Header.Get("Authorization"); got != "Bearer test-access-token" {
		t.Errorf("Authorization = %q, want Bearer test-access-token", got)
	}
	if tokenCalls != 1 {
		t.Errorf("token endpoint called %d times, want 1", tokenCalls)
	}

	// Second call should use cached token
	req2 := httptest.NewRequest(http.MethodPost, "/test", nil)
	if err := a.Apply(req2); err != nil {
		t.Fatalf("Apply (cached): %v", err)
	}
	if tokenCalls != 1 {
		t.Errorf("token endpoint called %d times after cache, want 1", tokenCalls)
	}
}

func TestOAuth2Auth_ExpiredRefreshes(t *testing.T) {
	var tokenCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "refreshed-token",
			"expires_in":   3600,
		})
	}))
	defer ts.Close()

	a := &oauth2Auth{
		clientID:     "c",
		clientSecret: "s",
		tokenURL:     ts.URL,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
	}
	// Pre-expire the token
	a.accessToken = "old-token"
	a.expiresAt = time.Now().Add(-1 * time.Hour)

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	if err := a.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer refreshed-token" {
		t.Errorf("Authorization = %q, want refreshed token", got)
	}
	if tokenCalls != 1 {
		t.Errorf("expected 1 token fetch, got %d", tokenCalls)
	}
}

func TestNewAuthenticator(t *testing.T) {
	decrypt := func(s string) string { return s }

	t.Run("none", func(t *testing.T) {
		a, err := NewAuthenticator("none", nil, decrypt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := a.(*noAuth); !ok {
			t.Errorf("expected *noAuth, got %T", a)
		}
	})

	t.Run("bearer", func(t *testing.T) {
		raw := json.RawMessage(`{"bearer_token":"tok123"}`)
		a, err := NewAuthenticator("bearer", raw, decrypt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := a.(*bearerAuth); !ok {
			t.Errorf("expected *bearerAuth, got %T", a)
		}
	})

	t.Run("basic", func(t *testing.T) {
		raw := json.RawMessage(`{"username":"u","password":"p"}`)
		a, err := NewAuthenticator("basic", raw, decrypt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := a.(*basicAuth); !ok {
			t.Errorf("expected *basicAuth, got %T", a)
		}
	})

	t.Run("oauth2", func(t *testing.T) {
		raw := json.RawMessage(`{"client_id":"c","client_secret":"s","token_url":"http://example.com/token"}`)
		a, err := NewAuthenticator("oauth2", raw, decrypt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := a.(*oauth2Auth); !ok {
			t.Errorf("expected *oauth2Auth, got %T", a)
		}
	})

	t.Run("sigv4", func(t *testing.T) {
		raw := json.RawMessage(`{"access_key_id":"AK","secret_access_key":"SK","region":"us-east-1","service":"s3"}`)
		a, err := NewAuthenticator("sigv4", raw, decrypt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := a.(*sigv4Auth); !ok {
			t.Errorf("expected *sigv4Auth, got %T", a)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		raw := json.RawMessage(`{}`)
		_, err := NewAuthenticator("kerberos", raw, decrypt, nil)
		if err == nil {
			t.Error("expected error for unsupported auth type")
		}
	})

	t.Run("decrypts sensitive fields", func(t *testing.T) {
		raw := json.RawMessage(`{"bearer_token":"enc://secret"}`)
		dec := func(s string) string { return strings.TrimPrefix(s, "enc://") }
		a, err := NewAuthenticator("bearer", raw, dec, nil)
		if err != nil {
			t.Fatal(err)
		}
		ba := a.(*bearerAuth)
		if ba.token != "secret" {
			t.Errorf("token = %q, want %q after decryption", ba.token, "secret")
		}
	})
}
