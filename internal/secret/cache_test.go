package secret

import (
	"testing"
	"time"
)

func TestCacheHitMiss(t *testing.T) {
	c := newSecretCache(5 * time.Minute)

	_, ok := c.Get("missing")
	if ok {
		t.Error("expected cache miss for missing key")
	}

	c.Set("key1", "value1")
	val, ok := c.Get("key1")
	if !ok || val != "value1" {
		t.Errorf("expected cache hit, got %q, %v", val, ok)
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	c := newSecretCache(1 * time.Millisecond)
	c.Set("key1", "value1")

	time.Sleep(5 * time.Millisecond)
	_, ok := c.Get("key1")
	if ok {
		t.Error("expected cache miss after TTL expiry")
	}
}

func TestCacheInvalidate(t *testing.T) {
	c := newSecretCache(5 * time.Minute)
	c.Set("key1", "value1")
	c.Invalidate("key1")

	_, ok := c.Get("key1")
	if ok {
		t.Error("expected cache miss after invalidation")
	}
}

func TestCacheInvalidateAll(t *testing.T) {
	c := newSecretCache(5 * time.Minute)
	c.Set("a", "1")
	c.Set("b", "2")
	c.InvalidateAll()

	_, okA := c.Get("a")
	_, okB := c.Get("b")
	if okA || okB {
		t.Error("expected all entries cleared after InvalidateAll")
	}
}
