package service

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newIdemTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// TestIdempotency_TryAcquireExclusive verifies the processing marker admits
// exactly one concurrent execution per (apiKeyID, key).
func TestIdempotency_TryAcquireExclusive(t *testing.T) {
	c := NewIdempotencyCache(newIdemTestRedis(t))
	ctx := context.Background()

	if !c.TryAcquire(ctx, 1, "key-a") {
		t.Fatal("first acquire must succeed")
	}
	if c.TryAcquire(ctx, 1, "key-a") {
		t.Fatal("second concurrent acquire must fail")
	}
	// Different key / different api key are independent.
	if !c.TryAcquire(ctx, 1, "key-b") {
		t.Fatal("different key must acquire")
	}
	if !c.TryAcquire(ctx, 2, "key-a") {
		t.Fatal("different api key must acquire")
	}

	// Release makes the key immediately reusable.
	c.Release(ctx, 1, "key-a")
	if !c.TryAcquire(ctx, 1, "key-a") {
		t.Fatal("acquire after release must succeed")
	}
}

// TestIdempotency_ConcurrentAcquireSingleWinner verifies the SETNX holds under
// concurrency: of N racing acquires, exactly one wins.
func TestIdempotency_ConcurrentAcquireSingleWinner(t *testing.T) {
	c := NewIdempotencyCache(newIdemTestRedis(t))
	ctx := context.Background()

	const n = 20
	var mu sync.Mutex
	winners := 0
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.TryAcquire(ctx, 7, "race") {
				mu.Lock()
				defer mu.Unlock()
				winners++
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("expected exactly 1 winner among %d racers, got %d", n, winners)
	}
}

// TestIdempotency_GetMissDuringProcessing verifies a processing marker does
// not leak into the replay cache: Get still reports a miss while in flight.
func TestIdempotency_GetMissDuringProcessing(t *testing.T) {
	c := NewIdempotencyCache(newIdemTestRedis(t))
	ctx := context.Background()

	if !c.TryAcquire(ctx, 1, "key-x") {
		t.Fatal("acquire failed")
	}
	if _, ok := c.Get(ctx, 1, "key-x"); ok {
		t.Fatal("Get must miss while the processing marker is held")
	}

	// Completing the flow: store the result, release the marker.
	if err := c.Set(ctx, 1, "key-x", &CachedResponse{StatusCode: 200, Body: []byte(`{}`)}); err != nil {
		t.Fatalf("set: %v", err)
	}
	c.Release(ctx, 1, "key-x")
	cached, ok := c.Get(ctx, 1, "key-x")
	if !ok || cached.StatusCode != 200 {
		t.Fatalf("expected cached replay after Set, got ok=%v resp=%+v", ok, cached)
	}
}
