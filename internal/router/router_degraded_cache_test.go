package router

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingRepo makes FindByModelName fail like a pool that cannot dial, and
// counts attempts so tests can prove suppression + single-flight merging.
type failingRepo struct {
	calls int64 // atomic: probes run concurrently
	err   error
	block chan struct{} // non-nil: hold the first call until closed
}

func (m *failingRepo) FindByModelName(context.Context, string, int64) ([]model.ProviderModel, error) {
	n := atomic.AddInt64(&m.calls, 1)
	if m.block != nil && n == 1 {
		<-m.block // owner probe stalls; late arrivals must queue behind it
	}
	return nil, m.err
}

func newDegradedResolver(repo ProviderModelRepo) *Resolver {
	return NewResolver(provider.NewRegistry(), repo, provider.NewHealthTracker(),
		map[StrategyName]RoutingStrategy{StrategyWeightedRandom: &WeightedRandomStrategy{}},
		nil, nil, nil, nil)
}

// Ladder cap3/cap4 finding (F-4): a DB outage made EVERY request run a fresh
// mapping query through a pool that could not dial (345k 503s, each with its
// own DB attempt plus a usage write). One degradation must suppress further
// lookups for a short window instead of re-failing per request — including
// for CONCURRENT arrivals (review: serial tests prove nothing about races).
func TestResolver_Degraded_ConcurrentColdStartSingleProbe(t *testing.T) {
	repo := &failingRepo{err: errors.New("dial tcp: connectex: port exhaustion")}
	r := newDegradedResolver(repo)

	const n = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = r.Resolve(context.Background(), "m", 0)
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.ErrorIs(t, err, ErrResolveDegraded)
	}
	assert.EqualValues(t, 1, atomic.LoadInt64(&repo.calls),
		"cold-start burst must collapse into ONE DB probe via single-flight, not %d", repo.calls)
}

func TestResolver_Degraded_ConcurrentBurstInsideWindow(t *testing.T) {
	repo := &failingRepo{err: errors.New("connection refused")}
	r := newDegradedResolver(repo)

	_, err := r.Resolve(context.Background(), "m", 0)
	require.ErrorIs(t, err, ErrResolveDegraded)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Resolve(context.Background(), "m", 0)
			assert.ErrorIs(t, err, ErrResolveDegraded)
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, atomic.LoadInt64(&repo.calls),
		"burst inside the suppression window must not touch the DB")
}

// The owner probe stalls (a slow, failing DB); late arrivals for the same key
// must queue behind it and share the outcome rather than piling on.
func TestResolver_Degraded_LateArrivalsJoinInFlightProbe(t *testing.T) {
	repo := &failingRepo{err: errors.New("i/o timeout"), block: make(chan struct{})}
	r := newDegradedResolver(repo)

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.Resolve(context.Background(), "m", 0)
		}(i)
	}
	time.Sleep(100 * time.Millisecond) // let the owner block inside the repo
	close(repo.block)
	wg.Wait()

	for _, err := range errs {
		require.ErrorIs(t, err, ErrResolveDegraded)
	}
	assert.EqualValues(t, 1, atomic.LoadInt64(&repo.calls),
		"late arrivals must join the in-flight probe, not start their own")
}

// Bounded staleness: after the window expires the next lookup re-attempts the
// DB (recovery latency is capped by the TTL).
func TestResolver_Degraded_WindowExpiryRetries(t *testing.T) {
	repo := &failingRepo{err: errors.New("dial refused")}
	r := newDegradedResolver(repo)

	_, err := r.Resolve(context.Background(), "m", 0)
	require.ErrorIs(t, err, ErrResolveDegraded)
	require.EqualValues(t, 1, atomic.LoadInt64(&repo.calls))

	time.Sleep(resolveDegradedTTL + 50*time.Millisecond)
	_, err = r.Resolve(context.Background(), "m", 0)
	require.ErrorIs(t, err, ErrResolveDegraded)
	assert.EqualValues(t, 2, atomic.LoadInt64(&repo.calls), "expired suppression must re-attempt the DB")
}

// Review defect: the negative cache must be BOUNDED — continuously varying
// (model,org) keys must not grow the map without limit.
func TestResolver_Degraded_CacheIsBounded(t *testing.T) {
	origCap := degradedCacheCap
	degradedCacheCap = 50
	defer func() { degradedCacheCap = origCap }()

	r := newDegradedResolver(&failingRepo{err: errors.New("refused")})
	for i := 0; i < degradedCacheCap+200; i++ {
		r.markDegraded(fmt.Sprintf("model-%d:%d", i, i))
	}
	r.degradedMu.Lock()
	size := len(r.degraded)
	r.degradedMu.Unlock()
	assert.LessOrEqual(t, size, degradedCacheCap,
		"negative cache must stay at/below cap under key churn (got %d)", size)
}

// Single-flight also collapses concurrent SUCCESS probes (cold start on a
// healthy DB: one query, everyone shares the routes).
func TestResolver_ConcurrentColdStartSuccessSingleProbe(t *testing.T) {
	repo := &countingRepo{data: map[string][]model.ProviderModel{
		"m": {
			{ID: 1, ProviderModel: "glm-a", Weight: 1, Status: 1,
				Provider: model.Provider{Name: "zhipu", Status: 1}},
		},
	}}
	reg := provider.NewRegistry()
	reg.Register("zhipu", &mockProvider{name: "zhipu"})
	r := NewResolver(reg, repo, provider.NewHealthTracker(),
		map[StrategyName]RoutingStrategy{StrategyWeightedRandom: &WeightedRandomStrategy{}},
		nil, nil, nil, nil)

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			routes, err := r.Resolve(context.Background(), "m", 0)
			require.NoError(t, err)
			assert.NotEmpty(t, routes)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, repo.calls, "concurrent cold-start successes must collapse into one DB query")
}
