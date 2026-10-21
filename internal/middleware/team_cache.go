package middleware

import (
	"context"
	"sync"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
)

type TeamCache struct {
	mu          sync.RWMutex
	items       map[int64]*teamCacheEntry
	repo        *repository.TeamRepo
	ttl         time.Duration
	staleWindow time.Duration
}

type teamCacheEntry struct {
	team      *model.Team
	expiresAt time.Time
}

func NewTeamCache(repo *repository.TeamRepo, ctx context.Context) *TeamCache {
	tc := &TeamCache{
		items:       make(map[int64]*teamCacheEntry),
		repo:        repo,
		ttl:         30 * time.Second,
		staleWindow: 10 * time.Minute,
	}
	go tc.evictLoop(ctx)
	return tc
}

func (tc *TeamCache) Get(ctx context.Context, teamID int64) *model.Team {
	tc.mu.RLock()
	if entry, ok := tc.items[teamID]; ok && entry.expiresAt.After(time.Now()) {
		tc.mu.RUnlock()
		return entry.team
	}
	tc.mu.RUnlock()

	team, err := tc.repo.GetByID(ctx, teamID)
	if err != nil {
		return nil
	}

	tc.mu.Lock()
	tc.items[teamID] = &teamCacheEntry{team: team, expiresAt: time.Now().Add(tc.ttl)}
	tc.mu.Unlock()

	return team
}

func (tc *TeamCache) evictLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tc.evictExpired()
		}
	}
}

func (tc *TeamCache) evictExpired() {
	now := time.Now()
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for id, entry := range tc.items {
		// Keep expired entries until past the LKG horizon (ttl+staleWindow)
		// so an outage right after TTL expiry still has a snapshot.
		if now.Sub(entry.expiresAt) > tc.staleWindow+tc.ttl {
			delete(tc.items, id)
		}
	}
}

// GetSafe is the enforcement-safe lookup for the budget middleware
// (2026-10-21 review: key-only admission on metadata-miss was an
// ENFORCEMENT bypass, 53 requests in r61's PG-stop window). Semantics:
//
//	"fresh"       – cache hit or DB lookup succeeded (the row may be at
//	                most ttl old; limit changes propagate within ttl).
//	"stale_lkg"   – the lookup FAILED (DB unreachable or row not found)
//	                but a last-known-good snapshot exists and its age is
//	                within ttl+staleWindow. The caller MUST still enforce
//	                against the snapshot's limit and include the scope in
//	                reserve/settle — the Redis counters are live; only the
//	                LIMIT value may lag by at most ttl+staleWindow (the
//	                documented revocation bound).
//	"unavailable" – no usable snapshot (true cold cache during an
//	                outage). The caller MUST fail closed: admitting
//	        key-only is the bypass this replaces.
//
// Expired entries are retained (not evicted) until they age past
// ttl+staleWindow so the LKG survives TTL expiry during an outage.
const (
	CacheModeFresh       = "fresh"
	CacheModeStaleLKG    = "stale_lkg"
	CacheModeUnavailable = "unavailable"
)

// SetStaleWindow arms the last-known-good horizon (default 10m). Wiring:
// gateway.budget_metadata_stale_window.
func (tc *TeamCache) SetStaleWindow(d time.Duration) {
	tc.mu.Lock()
	tc.staleWindow = d
	tc.mu.Unlock()
}

func (tc *TeamCache) GetSafe(ctx context.Context, teamID int64) (team *model.Team, mode string) {
	now := time.Now()
	tc.mu.RLock()
	entry, ok := tc.items[teamID]
	sw := tc.staleWindow
	tc.mu.RUnlock()
	if ok && entry.expiresAt.After(now) {
		return entry.team, CacheModeFresh
	}
	if t, err := tc.repo.GetByID(ctx, teamID); err == nil {
		tc.mu.Lock()
		tc.items[teamID] = &teamCacheEntry{team: t, expiresAt: now.Add(tc.ttl)}
		tc.mu.Unlock()
		return t, CacheModeFresh
	}
	// Lookup failed: last-known-good within ttl+staleWindow keeps
	// enforcement running (fail-safe direction — a possibly-stale LIMIT is
	// still enforced against exact live counters).
	if ok && now.Sub(entry.expiresAt) <= sw+tc.ttl {
		return entry.team, CacheModeStaleLKG
	}
	return nil, CacheModeUnavailable
}
