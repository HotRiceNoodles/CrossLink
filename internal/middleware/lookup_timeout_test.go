package middleware

// r78 availability-bound regressions (2026-10-22): a cache-miss lookup
// against an unreachable DB must release in BOUNDED time and fall through to
// the SAME fail-safe chain — cold = typed-unavailable (caller fails closed),
// expired-within-window = stale LKG keeps enforcing. A pre-fix hang here
// would hold the request (and its admission slot) for the full driver dial
// timeout (the r61 slot-exhaustion mechanism).
import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
)

// hangingTeamRepo blocks until the lookup context is done, then fails — a
// deterministic stand-in for a DB dial that cannot connect.
type hangingTeamRepo struct{ calls int }

func (h *hangingTeamRepo) GetByID(ctx context.Context, id int64) (*model.Team, error) {
	h.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

type hangingOrgRepo struct{ calls int }

func (h *hangingOrgRepo) GetByID(ctx context.Context, id int64) (*model.Organization, error) {
	h.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestGetSafe_LookupTimeout_ColdFailsClosedBounded(t *testing.T) {
	repo := &hangingTeamRepo{}
	tc := NewTeamCache(repo, context.Background())
	tc.SetLookupTimeout(120 * time.Millisecond)

	start := time.Now()
	team, mode := tc.GetSafe(context.Background(), 42)
	elapsed := time.Since(start)

	if team != nil || mode != CacheModeUnavailable {
		t.Fatalf("cold+hang must fail closed: got team=%v mode=%s", team, mode)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("lookup not bounded: took %s (want ~120ms)", elapsed)
	}
	if repo.calls != 1 {
		t.Fatalf("expected exactly 1 lookup, got %d", repo.calls)
	}

	// Org mirror.
	orepo := &hangingOrgRepo{}
	oc := NewOrgCache(orepo, context.Background())
	oc.SetLookupTimeout(120 * time.Millisecond)
	start = time.Now()
	org, omode := oc.GetSafe(context.Background(), 7)
	if org != nil || omode != CacheModeUnavailable {
		t.Fatalf("org cold+hang must fail closed: got org=%v mode=%s", org, omode)
	}
	if time.Since(start) >= 2*time.Second {
		t.Fatalf("org lookup not bounded")
	}
}

func TestGetSafe_LookupTimeout_LKGStillEnforcesBounded(t *testing.T) {
	// fastTeamRepo serves the first (cache-filling) lookup, then flips to
	// hanging for the outage-window lookup.
	repo := &switchableTeamRepo{}
	tc := NewTeamCache(repo, context.Background())
	tc.SetStaleWindow(10 * time.Minute)
	// Seed with a SHORT TTL so the fresh entry expires quickly but stays
	// inside ttl+staleWindow for the LKG window (SetTTL only affects entries
	// written after it).
	tc.SetTTL(1 * time.Millisecond)

	if _, mode := tc.GetSafe(context.Background(), 42); mode != CacheModeFresh {
		t.Fatalf("seed lookup must be fresh")
	}
	time.Sleep(5 * time.Millisecond) // fresh entry expires, LKG window opens
	repo.hang = true
	tc.SetLookupTimeout(120 * time.Millisecond)

	start := time.Now()
	team, mode := tc.GetSafe(context.Background(), 42)
	elapsed := time.Since(start)

	if team == nil || mode != CacheModeStaleLKG {
		t.Fatalf("expired-within-window + hang must serve LKG: got team=%v mode=%s", team, mode)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("LKG path not bounded: took %s", elapsed)
	}
}

type switchableTeamRepo struct{ hang bool }

func (s *switchableTeamRepo) GetByID(ctx context.Context, id int64) (*model.Team, error) {
	if s.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &model.Team{ID: id, Name: "t", BudgetLimit: 1}, nil
}
