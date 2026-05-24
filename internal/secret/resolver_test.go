package secret

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// mockStore is a test SecretStore that returns a fixed value.
type mockStore struct {
	name     string
	value    string
	err      error
	callCount atomic.Int32
}

func (m *mockStore) Name() string { return m.name }
func (m *mockStore) GetSecret(_ context.Context, key string) (string, error) {
	m.callCount.Add(1)
	if m.err != nil {
		return "", m.err
	}
	return m.value, nil
}

func TestResolvePlaintextPassthrough(t *testing.T) {
	r := NewSecretResolver(5 * time.Minute)
	val, err := r.Resolve(context.Background(), "sk-abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "sk-abc123" {
		t.Errorf("got %q, want %q", val, "sk-abc123")
	}
}

func TestResolveEmptyString(t *testing.T) {
	r := NewSecretResolver(5 * time.Minute)
	val, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "" {
		t.Errorf("got %q, want empty", val)
	}
}

func TestResolveViaStore(t *testing.T) {
	r := NewSecretResolver(5 * time.Minute)
	r.Register(&mockStore{name: "test", value: "resolved-value"})

	val, err := r.Resolve(context.Background(), "test://my-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "resolved-value" {
		t.Errorf("got %q, want %q", val, "resolved-value")
	}
}

func TestResolveCacheHit(t *testing.T) {
	store := &mockStore{name: "test", value: "cached-value"}
	r := NewSecretResolver(5 * time.Minute)
	r.Register(store)

	r.Resolve(context.Background(), "test://key1")
	r.Resolve(context.Background(), "test://key1")

	if store.callCount.Load() != 1 {
		t.Errorf("expected 1 GetSecret call, got %d", store.callCount.Load())
	}
}

func TestResolveUnknownScheme(t *testing.T) {
	r := NewSecretResolver(5 * time.Minute)
	_, err := r.Resolve(context.Background(), "unknown://key")
	if err == nil {
		t.Error("expected error for unknown scheme")
	}
}

func TestResolveExtraConfigSecrets(t *testing.T) {
	r := NewSecretResolver(5 * time.Minute)
	r.Register(&mockStore{name: "env", value: "AKIAIOSFODNN7EXAMPLE"})

	config := map[string]any{
		"access_key_id":     "env://AWS_ACCESS_KEY_ID",
		"region":            "us-east-1",
		"secret_access_key": "env://AWS_SECRET_ACCESS_KEY",
	}

	err := r.ResolveExtraConfigSecrets(context.Background(), config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if config["access_key_id"] != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("access_key_id not resolved: %v", config["access_key_id"])
	}
	if config["region"] != "us-east-1" {
		t.Errorf("region should not be changed: %v", config["region"])
	}
	if config["secret_access_key"] != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("secret_access_key not resolved: %v", config["secret_access_key"])
	}
}

func TestResolveExtraConfigNonReferenceSkipped(t *testing.T) {
	r := NewSecretResolver(5 * time.Minute)

	config := map[string]any{
		"access_key_id": "plaintext-key",
		"region":        "us-east-1",
	}

	err := r.ResolveExtraConfigSecrets(context.Background(), config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config["access_key_id"] != "plaintext-key" {
		t.Error("plaintext value should not be modified")
	}
}

func TestResolverStores(t *testing.T) {
	r := NewSecretResolver(5 * time.Minute)
	r.Register(&mockStore{name: "env"})
	r.Register(&mockStore{name: "enc"})

	stores := r.Stores()
	if len(stores) != 2 {
		t.Errorf("expected 2 stores, got %d", len(stores))
	}
}

func TestResolverInvalidateCache(t *testing.T) {
	store := &mockStore{name: "test", value: "value1"}
	r := NewSecretResolver(5 * time.Minute)
	r.Register(store)

	r.Resolve(context.Background(), "test://key1")
	r.InvalidateCache()
	r.Resolve(context.Background(), "test://key1")

	if store.callCount.Load() != 2 {
		t.Errorf("expected 2 GetSecret calls after invalidation, got %d", store.callCount.Load())
	}
}
