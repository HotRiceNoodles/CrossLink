package provider

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// M1 acceptance gap: the ≤2s cross-instance invalidation target, exercised
// over a REAL pub/sub round trip (miniredis) between two RegistrySync
// instances — instance A publishes invalidate_model, instance B's injected
// callback must fire within the window. (miniredis is in-process; production
// adds network latency on top, which the 2s budget absorbs.)
func TestRegistrySync_ModelInvalidationCrossInstanceLatency(t *testing.T) {
	// Timing test: NOT parallel — the 2s acceptance window measures pub/sub
	// delivery, not scheduler jitter from sibling tests under a loaded suite.
	// t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	invalidated := make(chan string, 4)
	syncB := NewRegistrySync(rdb, NewRegistry(), nil, nil)
	syncB.OnModelInvalidated = func(name string) { invalidated <- name }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go syncB.Start(ctx)

	syncA := NewRegistrySync(rdb, NewRegistry(), nil, nil)

	// Readiness: subscription must be live before we measure. Probe with a
	// throwaway model name and drain it.
	syncA.NotifyModelInvalidated("__probe__")
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case n := <-invalidated:
			if n == "__probe__" {
				goto ready
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("subscriber not ready within 2s")
		}
	}
ready:

	start := time.Now()
	syncA.NotifyModelInvalidated("m-target")
	select {
	case n := <-invalidated:
		require.Equal(t, "m-target", n)
		require.LessOrEqual(t, time.Since(start), 2*time.Second,
			"cross-instance invalidate_model must apply within the 2s acceptance window")
	case <-time.After(3 * time.Second):
		t.Fatal("invalidate_model was not applied within 3s")
	}
}
