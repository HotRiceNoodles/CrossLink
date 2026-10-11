package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/redis/go-redis/v9"
)

func TestValidate_CachedDisabledKeyRejected(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatalf("crypto provider: %v", err)
	}
	keyPlain := "secret-key-disabled"
	cacheKey := "auth:key:" + cp.HashHex([]byte(keyPlain))

	// Seed the auth cache with a key that was disabled after caching.
	disabled := &model.APIKey{ID: 7, KeyPrefix: "sk-x", Status: 2}
	b, err := json.Marshal(disabled)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := rdb.Set(context.Background(), cacheKey, b, time.Minute).Err(); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	svc := NewKeyService(nil, nil, nil, cp, rdb)
	if _, err := svc.Validate(context.Background(), keyPlain); !errors.Is(err, repository.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound for cached disabled key, got %v", err)
	}
	// The stale entry must be dropped so subsequent requests hit the DB path.
	if n := rdb.Exists(context.Background(), cacheKey).Val(); n != 0 {
		t.Errorf("stale cache entry not removed (exists=%d)", n)
	}
}

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
