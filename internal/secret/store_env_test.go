package secret

import (
	"context"
	"testing"
)

func TestEnvStoreGetSecret(t *testing.T) {
	t.Setenv("TEST_SECRET_KEY", "my-secret-value")

	s := NewEnvSecretStore()
	val, err := s.GetSecret(context.Background(), "TEST_SECRET_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "my-secret-value" {
		t.Errorf("got %q, want %q", val, "my-secret-value")
	}
}

func TestEnvStoreMissingKey(t *testing.T) {
	s := NewEnvSecretStore()
	_, err := s.GetSecret(context.Background(), "NONEXISTENT_SECRET_12345")
	if err == nil {
		t.Error("expected error for missing env var")
	}
}

func TestEnvStoreName(t *testing.T) {
	s := NewEnvSecretStore()
	if s.Name() != "env" {
		t.Errorf("Name() = %q, want %q", s.Name(), "env")
	}
}
