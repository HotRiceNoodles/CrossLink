package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ctxAwareRepo wraps a slow repo whose in-flight query can be cancelled via
// the caller's ctx, exactly like a real DB driver: the call returns as soon
// as EITHER the result is produced OR the passed ctx fires.
type ctxAwareRepo struct {
	calls   int64
	failN   int64 // first failN calls return a DB error (healthy-DB failure simulation)
	err     error
	release chan struct{} // close to let the next call succeed
	resumed int64
}

func (m *ctxAwareRepo) FindByModelName(ctx context.Context, _ string, _ int64) ([]model.ProviderModel, error) {
	atomic.AddInt64(&m.calls, 1)
	if atomic.LoadInt64(&m.calls) <= m.failN {
		return nil, m.err
	}
	select {
	case <-m.release:
		atomic.AddInt64(&m.resumed, 1)
		return []model.ProviderModel{
			{ID: 1, ProviderModel: "glm-a", Weight: 1, Status: 1,
				Provider: model.Provider{Name: "zhipu", Status: 1}},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newCtxResolver(repo ProviderModelRepo) *Resolver {
	reg := provider.NewRegistry()
	reg.Register("zhipu", &mockProvider{name: "zhipu"})
	return NewResolver(reg, repo, provider.NewHealthTracker(),
		map[StrategyName]RoutingStrategy{StrategyWeightedRandom: &WeightedRandomStrategy{}},
		nil, nil, nil, nil)
}

func degradedSize(r *Resolver) int {
	r.degradedMu.Lock()
	defer r.degradedMu.Unlock()
	return len(r.degraded)
}

// Review directive: a waiter pinned behind an in-flight probe MUST return as
// soon as ITS OWN ctx is cancelled — not when the owner gets around to
// finishing.
func TestProbeResolve_WaiterReturnsOnOwnCtxCancel(t *testing.T) {
	repo := &ctxAwareRepo{release: make(chan struct{})}
	r := newCtxResolver(repo)

	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_, err := r.Resolve(context.Background(), "m", 0)
		assert.NoError(t, err) // owner completes when release closes below
	}()
	time.Sleep(100 * time.Millisecond) // owner is now blocked inside the repo

	wctx, wcancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer wcancel()
	start := time.Now()
	_, err := r.Resolve(wctx, "m", 0)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, time.Second, "waiter must return on its own deadline, not on the owner's progress")
	assert.EqualValues(t, 1, atomic.LoadInt64(&repo.calls), "the cancelled waiter must not start a second DB query")

	close(repo.release) // let the owner finish; nothing was polluted
	<-ownerDone
	assert.Equal(t, 0, degradedSize(r), "a waiter's cancellation must not mark degradation")
}

// Review directive: an owner cancelled mid-probe must not poison the negative
// cache for OTHER requests when the DB is healthy — live waiters retry and
// get the real outcome.
func TestProbeResolve_OwnerCancelDoesNotPolluteHealthyDB(t *testing.T) {
	repo := &ctxAwareRepo{release: make(chan struct{})}
	r := newCtxResolver(repo)

	octx, ocancel := context.WithCancel(context.Background())
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_, err := r.Resolve(octx, "m", 0)
		assert.ErrorIs(t, err, context.Canceled)
	}()
	time.Sleep(100 * time.Millisecond) // owner blocked in the repo
	ocancel()                          // the OWNER's request goes away
	<-ownerDone

	assert.Equal(t, 0, degradedSize(r),
		"owner cancellation is not a DB verdict — the negative cache must stay clean")

	// A live caller right after must get the REAL (healthy) outcome, not an
	// inherited cancellation or a suppression window.
	close(repo.release)
	routes, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	assert.NotEmpty(t, routes)
	assert.EqualValues(t, 2, atomic.LoadInt64(&repo.calls),
		"post-cancellation caller re-probes (the cancelled probe carried no verdict)")
	assert.Equal(t, 0, degradedSize(r))
}

// Concurrent chaos: N callers, some cancel mid-wait, the rest survive — the
// survivors converge on the healthy outcome, cancellations leak nothing.
func TestProbeResolve_ConcurrentCancelChaosConverges(t *testing.T) {
	repo := &ctxAwareRepo{release: make(chan struct{})}
	r := newCtxResolver(repo)

	const n = 24
	var wg sync.WaitGroup
	var survived, cancelled int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			var cancel context.CancelFunc
			if i%3 == 0 {
				ctx, cancel = context.WithTimeout(ctx, 120*time.Millisecond)
				defer cancel()
			}
			_, err := r.Resolve(ctx, "m", 0)
			if ctx.Err() != nil {
				atomic.AddInt64(&cancelled, 1)
				assert.ErrorIs(t, err, ctx.Err())
			} else {
				atomic.AddInt64(&survived, 1)
				assert.NoError(t, err)
			}
		}(i)
	}
	time.Sleep(200 * time.Millisecond) // let cancellations fire mid-wait
	close(repo.release)                // DB answers
	wg.Wait()

	assert.EqualValues(t, n/3*1, cancelled, "one third of callers cancelled (i%3==0)")
	assert.EqualValues(t, n-n/3, survived, "survivors all got the healthy outcome")
	assert.Equal(t, 0, degradedSize(r), "cancellation chaos must leave the negative cache empty")
	assert.LessOrEqual(t, atomic.LoadInt64(&repo.calls), int64(4),
		"cancellations must not fan out into extra DB probes")
}

// Degradation marking still works for REAL DB failures and recovers after
// the TTL (regression guard for the ctx changes).
func TestProbeResolve_RealFailureStillMarksAndRecovers(t *testing.T) {
	repo := &ctxAwareRepo{failN: 1, err: errors.New("connection refused"), release: make(chan struct{})}
	close(repo.release)
	r := newCtxResolver(repo)

	_, err := r.Resolve(context.Background(), "m", 0)
	require.ErrorIs(t, err, ErrResolveDegraded)
	assert.Equal(t, 1, degradedSize(r), "a real DB failure marks degradation")

	time.Sleep(resolveDegradedTTL + 50*time.Millisecond)
	routes, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	assert.NotEmpty(t, routes, "after the window the healthy DB serves again")
	assert.EqualValues(t, 2, atomic.LoadInt64(&repo.calls))
	assert.Equal(t, 0, degradedSize(r), "success clears the suppression")
}
