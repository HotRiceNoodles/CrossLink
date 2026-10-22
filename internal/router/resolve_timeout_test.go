package router

// r78 availability-bound regression (2026-10-22): resolveUncached's DB round
// trip is bounded; a hanging mapping lookup reaches the degraded
// negative-cache path (ErrResolveDegraded) in bounded time, and the window
// suppresses further DB access — pre-fix, the single-flight probe (and every
// request sharing it) sat on the driver dial timeout.
import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
)

type hangingProviderRepo struct{ calls int }

func (h *hangingProviderRepo) FindByModelName(ctx context.Context, modelName string, orgID int64) ([]model.ProviderModel, error) {
	h.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestResolve_LookupTimeout_DegradedBoundedAndSuppressed(t *testing.T) {
	repo := &hangingProviderRepo{}
	r := NewResolver(nil, repo, nil, nil, nil, nil, nil, nil)
	r.SetResolveTimeout(120 * time.Millisecond)

	start := time.Now()
	_, err := r.Resolve(context.Background(), "cold-model", 1)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrResolveDegraded) {
		t.Fatalf("hanging lookup must surface ErrResolveDegraded, got %v", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("resolve not bounded: took %s (want ~120ms + overhead)", elapsed)
	}
	if repo.calls != 1 {
		t.Fatalf("expected exactly 1 DB probe, got %d", repo.calls)
	}

	// Inside the degradation window a second resolve must answer from the
	// negative cache WITHOUT touching the repo, still ErrResolveDegraded.
	start = time.Now()
	_, err = r.Resolve(context.Background(), "cold-model", 1)
	if !errors.Is(err, ErrResolveDegraded) {
		t.Fatalf("second resolve must be degraded, got %v", err)
	}
	if time.Since(start) >= 100*time.Millisecond {
		t.Fatalf("degraded-cache answer not fast")
	}
	if repo.calls != 1 {
		t.Fatalf("degradation window failed to suppress the DB probe (calls=%d)", repo.calls)
	}
}

func TestResolve_SetResolveTimeoutDefaultApplied(t *testing.T) {
	r := NewResolver(nil, nil, nil, nil, nil, nil, nil, nil)
	if d := time.Duration(r.resolveTimeout.Load()); d != 2*time.Second {
		t.Fatalf("default resolve timeout = %s, want 2s", d)
	}
	r.SetResolveTimeout(0) // non-positive must not clobber the default
	if d := time.Duration(r.resolveTimeout.Load()); d != 2*time.Second {
		t.Fatalf("zero SetResolveTimeout must keep the default, got %s", d)
	}
}
