package middleware

import (
	"context"
	"sync"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
)

type TeamCache struct {
	mu    sync.RWMutex
	items map[int64]*teamCacheEntry
	repo  *repository.TeamRepo
	ttl   time.Duration
}

type teamCacheEntry struct {
	team      *model.Team
	expiresAt time.Time
}

func NewTeamCache(repo *repository.TeamRepo, ctx context.Context) *TeamCache {
	tc := &TeamCache{
		items: make(map[int64]*teamCacheEntry),
		repo:  repo,
		ttl:   30 * time.Second,
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
		if entry.expiresAt.Before(now) {
			delete(tc.items, id)
		}
	}
}
