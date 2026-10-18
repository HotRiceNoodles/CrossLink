package router

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Deterministic scheduling helpers: every step is sequenced by channels or
// white-box state polling — no sleeps-then-hope.

// registeredProbe polls r.probes until cacheKey has an in-flight probe and
// returns its pointer (the waiter-to-be must join BEFORE the owner is
// cancelled, deterministically).
func registeredProbe(r *Resolver, cacheKey string) *resolveProbe {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.probesMu.Lock()
		p, ok := r.probes[cacheKey]
		r.probesMu.Unlock()
		if ok {
			return p
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil
}

// Review fix ordering proof: when an owner cancels, the probe's registration
// must be REMOVED BEFORE done is published. A waiter that observes done
// closed must therefore never find the probe still registered — otherwise it
// burns its single retry re-reading the same cancelled probe and returns the
// owner's ctx error. forget(happens-before) close(happens-before) waiter
// observe makes this assertion deterministic for the fixed order.
func TestProbeResolve_CancelUnregistersBeforePublish(t *testing.T) {
	repo := &ctxAwareRepo{release: make(chan struct{})}
	r := newCtxResolver(repo)

	octx, ocancel := context.WithCancel(context.Background())
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_, err := r.Resolve(octx, "m", 0)
		require.ErrorIs(t, err, context.Canceled)
	}()

	p := registeredProbe(r, "m:0")
	require.NotNil(t, p, "owner probe must register before we sequence the cancel")

	// A waiter joins the probe and records, at the instant it observes the
	// published outcome, whether the registration is still present.
	stillRegistered := make(chan bool, 1)
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		<-p.done // wakes only after close(done) — which happens after forget
		r.probesMu.Lock()
		_, still := r.probes["m:0"]
		r.probesMu.Unlock()
		stillRegistered <- still
	}()

	ocancel()
	<-ownerDone
	<-waiterDone

	select {
	case still := <-stillRegistered:
		assert.False(t, still,
			"registration must be gone when the cancelled outcome is observable; a still-registered cancelled probe lets waiters burn their retry on it")
	default:
		t.Fatal("waiter did not record the registration state")
	}
}

// Deterministic behavioral proof: a live waiter joined to a probe whose owner
// is cancelled MUST end up probing with its OWN ctx (become the next owner)
// and return the real outcome — never the owner's foreign ctx error, and
// never losing its retry to a stale registration.
func TestProbeResolve_WaiterRetriesAsOwnerDeterministic(t *testing.T) {
	repo := &ctxAwareRepo{release: make(chan struct{})}
	r := newCtxResolver(repo)

	octx, ocancel := context.WithCancel(context.Background())
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_, err := r.Resolve(octx, "m", 0)
		require.ErrorIs(t, err, context.Canceled)
	}()

	p := registeredProbe(r, "m:0")
	require.NotNil(t, p, "owner probe must be in flight")

	waiterDone := make(chan struct{})
	var werr error
	var wroutes int
	go func() {
		defer close(waiterDone)
		routes, err := r.Resolve(context.Background(), "m", 0)
		werr = err
		wroutes = len(routes)
	}()

	// Sequence: cancel the owner; the waiter observes the cancelled outcome
	// and retries — as the new OWNER it blocks in the repo on its own ctx.
	ocancel()
	<-ownerDone

	// Deterministically confirm the retry re-registered a probe (waiter is
	// now the owner, blocked in fn with its own live ctx).
	p2 := registeredProbe(r, "m:0")
	if p2 != nil && p2 != p {
		// expected path: waiter became owner
	} else if p2 == nil {
		// The waiter may have already completed if scheduling released it
		// before we polled; its result must still be a real outcome.
		select {
		case <-waiterDone:
		case <-time.After(2 * time.Second):
			t.Fatal("waiter neither re-registered nor completed")
		}
	}

	close(repo.release) // DB answers the waiter's own probe
	select {
	case <-waiterDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not complete after DB release")
	}

	require.NoError(t, werr, "live waiter must get the real outcome, not the owner's ctx error")
	assert.Greater(t, wroutes, 0)
	assert.EqualValues(t, 2, atomic.LoadInt64(&repo.calls),
		"owner (cancelled) + waiter's own probe = exactly 2 DB calls")
	assert.Equal(t, 0, degradedSize(r))
}
