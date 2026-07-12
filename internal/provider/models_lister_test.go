package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestOpenAICompatible(t *testing.T, baseURL string) *OpenAICompatibleProvider {
	t.Helper()
	return NewOpenAICompatible("test", baseURL, 5*time.Second)
}

func TestListUpstreamModels_OpenAIEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("missing/bad auth header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o","owned_by":"openai"},{"id":"gpt-4o-mini","owned_by":"openai"}]}`)
	}))
	defer srv.Close()

	p := newTestOpenAICompatible(t, srv.URL)
	models, err := p.ListUpstreamModels(context.Background(), "sk-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "gpt-4o" || models[1].ID != "gpt-4o-mini" {
		t.Errorf("unexpected models: %+v", models)
	}
}

func TestListUpstreamModels_BareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"deepseek-chat"},{"id":"deepseek-coder"}]`)
	}))
	defer srv.Close()

	p := newTestOpenAICompatible(t, srv.URL)
	models, err := p.ListUpstreamModels(context.Background(), "sk-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 || models[0].ID != "deepseek-chat" {
		t.Errorf("unexpected models: %+v", models)
	}
}

func TestListUpstreamModels_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	p := newTestOpenAICompatible(t, srv.URL)
	models, err := p.ListUpstreamModels(context.Background(), "sk-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("expected 0 models, got %d", len(models))
	}
}

func TestListUpstreamModels_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	p := newTestOpenAICompatible(t, srv.URL)
	_, err := p.ListUpstreamModels(context.Background(), "bad")
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
}

func TestUpstreamModelJSON(t *testing.T) {
	// Round-trip a model to confirm JSON tags behave for the API response.
	m := UpstreamModel{ID: "gpt-4o", OwnedBy: "openai"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"id":"gpt-4o","owned_by":"openai"}`
	if string(b) != want {
		t.Errorf("json mismatch\ngot:  %s\nwant: %s", b, want)
	}
}
