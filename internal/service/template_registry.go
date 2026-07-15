package service

import (
	"context"
	"sync"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

// TemplateRegistry is an in-memory cache of active prompt_templates keyed by
// name. It lazy-loads from the DB on miss and caches for the process lifetime
// (zero steady-state DB load). TemplateRegistrySync (Redis Pub/Sub) invalidates
// entries across instances on CRUD. See
// docs/plans/2026-07-14-context-engineering-gateway-design.md.
//
// MVP scopes by name only (name is globally unique via partial index); enterprise
// org-scoping is applied via the AssemblerHook, not here.
type TemplateRegistry struct {
	db    *gorm.DB
	mu    sync.RWMutex
	cache map[string]*model.PromptTemplate
}

func NewTemplateRegistry(db *gorm.DB) *TemplateRegistry {
	return &TemplateRegistry{db: db, cache: make(map[string]*model.PromptTemplate)}
}

// Get returns the active (status=1, non-deleted) template by name, loading from
// the DB on cache miss. Returns (nil, false) if not found or disabled.
func (r *TemplateRegistry) Get(ctx context.Context, name string) (*model.PromptTemplate, bool) {
	r.mu.RLock()
	tpl, ok := r.cache[name]
	r.mu.RUnlock()
	if ok {
		return tpl, true
	}

	var m model.PromptTemplate
	err := r.db.WithContext(ctx).
		Where("name = ? AND deleted_at IS NULL AND status = 1", name).
		First(&m).Error
	if err != nil {
		return nil, false
	}
	r.mu.Lock()
	r.cache[name] = &m
	r.mu.Unlock()
	return &m, true
}

// Invalidate drops the cached entry so the next Get re-reads from the DB.
// Called on template update/delete and via Redis Pub/Sub from other instances.
func (r *TemplateRegistry) Invalidate(name string) {
	r.mu.Lock()
	delete(r.cache, name)
	r.mu.Unlock()
}
