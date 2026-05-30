package middleware

import (
	"context"
	"sync"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
)

type OrgCache struct {
	mu    sync.RWMutex
	items map[int64]*orgCacheEntry
	repo  *repository.OrgRepo
	ttl   time.Duration
}

type orgCacheEntry struct {
	org       *model.Organization
	expiresAt time.Time
}

func NewOrgCache(repo *repository.OrgRepo, ctx context.Context) *OrgCache {
	oc := &OrgCache{
		items: make(map[int64]*orgCacheEntry),
		repo:  repo,
		ttl:   30 * time.Second,
	}
	go oc.evictLoop(ctx)
	return oc
}

func (oc *OrgCache) Get(ctx context.Context, orgID int64) *model.Organization {
	oc.mu.RLock()
	if entry, ok := oc.items[orgID]; ok && entry.expiresAt.After(time.Now()) {
		oc.mu.RUnlock()
		return entry.org
	}
	oc.mu.RUnlock()

	org, err := oc.repo.GetByID(ctx, orgID)
	if err != nil {
		return nil
	}

	oc.mu.Lock()
	oc.items[orgID] = &orgCacheEntry{org: org, expiresAt: time.Now().Add(oc.ttl)}
	oc.mu.Unlock()

	return org
}

func (oc *OrgCache) evictLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			oc.evictExpired()
		}
	}
}

func (oc *OrgCache) evictExpired() {
	now := time.Now()
	oc.mu.Lock()
	defer oc.mu.Unlock()
	for id, entry := range oc.items {
		if entry.expiresAt.Before(now) {
			delete(oc.items, id)
		}
	}
}

func (oc *OrgCache) Invalidate(orgID int64) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	delete(oc.items, orgID)
}
