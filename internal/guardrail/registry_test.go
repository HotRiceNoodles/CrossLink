package guardrail

import (
	"context"
	"encoding/json"
	"testing"
)

// cleanupRegistry removes test entries from the global registry.
func cleanupRegistry(t *testing.T, types []string) {
	t.Helper()
	registryMu.Lock()
	for _, typ := range types {
		delete(engineFactories, typ)
	}
	registryMu.Unlock()
}

type mockEngine struct{}

func (m *mockEngine) Name() string { return "mock" }
func (m *mockEngine) Check(_ context.Context, _ string, _ Direction, _ string) (*GuardrailResult, error) {
	return &GuardrailResult{}, nil
}

func TestRegisterEngine_CreateEngine(t *testing.T) {
	typ := "test_mock_v1"
	RegisterEngine(typ, func(config json.RawMessage) (GuardrailEngine, error) {
		return &mockEngine{}, nil
	})
	defer cleanupRegistry(t, []string{typ})

	engine, err := CreateEngine(typ, nil)
	if err != nil {
		t.Fatalf("CreateEngine() error: %v", err)
	}
	if engine == nil {
		t.Fatal("expected non-nil engine")
	}

	_, err = CreateEngine("nonexistent_type", nil)
	if err == nil {
		t.Error("expected error for unknown type")
	}
}

func TestRegisterEngineV2_CreateEngine(t *testing.T) {
	typ := "test_mock_v2"
	RegisterEngineV2(typ, func(config json.RawMessage, deps EngineDeps) (GuardrailEngine, error) {
		return &mockEngine{}, nil
	})
	defer cleanupRegistry(t, []string{typ})

	engine, err := CreateEngine(typ, nil)
	if err != nil {
		t.Fatalf("CreateEngine() error: %v", err)
	}
	if engine == nil {
		t.Fatal("expected non-nil engine")
	}
}

func TestRegisteredTypes(t *testing.T) {
	types := []string{"test_type_a", "test_type_b"}
	for _, typ := range types {
		RegisterEngine(typ, func(config json.RawMessage) (GuardrailEngine, error) {
			return &mockEngine{}, nil
		})
	}
	defer cleanupRegistry(t, types)

	registered := RegisteredTypes()
	found := 0
	for _, typ := range types {
		for _, r := range registered {
			if r == typ {
				found++
				break
			}
		}
	}
	if found != len(types) {
		t.Errorf("found %d of %d registered types", found, len(types))
	}
}
