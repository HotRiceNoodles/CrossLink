package service

import (
	"testing"
)

func TestGenerateRawKey_Prefix(t *testing.T) {
	key, err := generateRawKey()
	if err != nil {
		t.Fatalf("generateRawKey: %v", err)
	}
	if len(key) < 10 {
		t.Errorf("key too short: %s", key)
	}
	if key[:3] != "cl-" {
		t.Errorf("key should start with cl-, got: %s", key[:3])
	}
}

func TestGenerateRawKey_Unique(t *testing.T) {
	keys := make(map[string]bool)
	for i := 0; i < 100; i++ {
		key, err := generateRawKey()
		if err != nil {
			t.Fatalf("generateRawKey: %v", err)
		}
		if keys[key] {
			t.Errorf("duplicate key generated: %s", key)
		}
		keys[key] = true
	}
}
