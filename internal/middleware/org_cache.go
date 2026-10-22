package middleware

import (
	"context"
	"sync"
	"time"

	"github.com/crosslink/internal/model"
)

// orgMetaRepo narrows what OrgCache needs (see teamMetaRepo for rationale).
type orgMetaRepo interface {
	GetByID(ctx context.Context, id int64) (*model.Organization, error)
}

type OrgCache struct {
	mu            sync.RWMutex
	items         map[int64]*orgCacheEntry
	repo          orgMetaRepo
	ttl           time.Duration
	staleWindow   time.Duration
	lookupTimeout time.Duration
}

type orgCacheEntry struct {
	org       *model.Organization
	expiresAt time.Time
}

func NewOrgCache(repo orgMetaRepo, ctx context.Context) *OrgCache {
	oc := &OrgCache{
		items:         make(map[int64]*orgCacheEntry),
		repo:          repo,
		ttl:           30 * time.Second,
		staleWindow:   10 * time.Minute,
		lookupTimeout: 2 * time.Second,
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
		// Keep expired entries until past the LKG horizon (ttl+staleWindow).
		if now.Sub(entry.expiresAt) > oc.staleWindow+oc.ttl {
			delete(oc.items, id)
		}
	}
}

func (oc *OrgCache) Invalidate(orgID int64) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	delete(oc.items, orgID)
}

// SetStaleWindow arms the org last-known-good horizon (see TeamCache.GetSafe).
func (oc *OrgCache) SetStaleWindow(d time.Duration) {
	oc.mu.Lock()
	oc.staleWindow = d
	oc.mu.Unlock()
}

// GetSafe mirrors TeamCache.GetSafe: fresh / stale_lkg (enforce against the
// snapshot, include the scope) / unavailable (caller MUST fail closed).
func (oc *OrgCache) GetSafe(ctx context.Context, orgID int64) (org *model.Organization, mode string) {
	now := time.Now()
	oc.mu.RLock()
	entry, ok := oc.items[orgID]
	sw := oc.staleWindow
	lt := oc.lookupTimeout
	oc.mu.RUnlock()
	if ok && entry.expiresAt.After(now) {
		return entry.org, CacheModeFresh
	}
	// r78 availability bound (mirrors TeamCache.GetSafe): bound the miss
	// lookup instead of holding the slot for the driver dial timeout; the
	// fallback chain below is unchanged.
	if lt <= 0 {
		lt = 2 * time.Second
	}
	lctx, cancel := context.WithTimeout(ctx, lt)
	o, err := oc.repo.GetByID(lctx, orgID)
	cancel()
	if err == nil {
		oc.mu.Lock()
		oc.items[orgID] = &orgCacheEntry{org: o, expiresAt: now.Add(oc.ttl)}
		oc.mu.Unlock()
		return o, CacheModeFresh
	}
	if ok && now.Sub(entry.expiresAt) <= sw+oc.ttl {
		return entry.org, CacheModeStaleLKG
	}
	return nil, CacheModeUnavailable
}

// SetLookupTimeout bounds the per-miss DB lookup (r78). Wiring:
// gateway.budget_metadata_lookup_timeout.
func (oc *OrgCache) SetLookupTimeout(d time.Duration) {
	oc.mu.Lock()
	oc.lookupTimeout = d
	oc.mu.Unlock()
}

// SetTTL overrides the fresh-entry TTL (see TeamCache.SetTTL).
func (oc *OrgCache) SetTTL(d time.Duration) {
	oc.mu.Lock()
	oc.ttl = d
	oc.mu.Unlock()
}
