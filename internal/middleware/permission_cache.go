package middleware

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/crosslink/internal/repository"
)

// PermissionCache stores role→action mappings in memory for fast access.
type PermissionCache struct {
	roleRepo *repository.RoleRepo
	mu       sync.RWMutex
	perms    map[int64]map[string]bool // roleID → action → allowed
}

func NewPermissionCache(roleRepo *repository.RoleRepo) *PermissionCache {
	pc := &PermissionCache{
		roleRepo: roleRepo,
		perms:    make(map[int64]map[string]bool),
	}
	return pc
}

// Load fetches all permissions from DB into memory.
func (pc *PermissionCache) Load() error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	ctx := context.Background()
	allPerms, err := pc.roleRepo.GetAllPermissions(ctx)
	if err != nil {
		return err
	}

	newPerms := make(map[int64]map[string]bool, len(allPerms))
	for roleID, actions := range allPerms {
		actionSet := make(map[string]bool, len(actions))
		for _, a := range actions {
			actionSet[a] = true
		}
		newPerms[roleID] = actionSet
	}
	pc.perms = newPerms
	slog.Info("permission cache loaded", "roles", len(newPerms))
	return nil
}

// HasPermission checks if a role has a specific action permission.
func (pc *PermissionCache) HasPermission(roleID int64, action string) bool {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	actions, ok := pc.perms[roleID]
	if !ok {
		return false
	}
	return actions[action]
}

// GetPermissions returns all actions for a role.
func (pc *PermissionCache) GetPermissions(roleID int64) []string {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	actions, ok := pc.perms[roleID]
	if !ok {
		return nil
	}
	result := make([]string, 0, len(actions))
	for a := range actions {
		result = append(result, a)
	}
	return result
}

// Invalidate reloads the cache from DB.
func (pc *PermissionCache) Invalidate() error {
	if err := pc.Load(); err != nil {
		slog.Error("failed to reload permission cache", "error", err)
		return err
	}
	return nil
}

// RunRefreshLoop starts a background goroutine that reloads permissions every interval.
// Stops when ctx is cancelled.
func (pc *PermissionCache) RunRefreshLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := pc.Load(); err != nil {
					slog.Warn("periodic permission cache reload failed", "error", err)
				}
			}
		}
	}()
}
