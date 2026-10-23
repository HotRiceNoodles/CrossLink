package service

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"gorm.io/datatypes"
)

// The provider_models config lookup inside GetModelCacheConfig is the fourth
// unbounded request-path DB call named by r78e (cache_service.go dial
// accepted then aborted only by client cancellation). These tests pin the
// bound and that success semantics are unchanged.
func TestGetModelCacheConfig_LookupTimeout_Bounded(t *testing.T) {
	s := &CacheService{}
	s.modelConfigReader = func(ctx context.Context, modelName string) (*model.ProviderModel, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s.SetModelConfigLookupTimeout(120 * time.Millisecond)

	start := time.Now()
	ttl, disabled := s.GetModelCacheConfig(context.Background(), "model-hang")
	elapsed := time.Since(start)

	if ttl != 0 || disabled {
		t.Fatalf("expected default policy (0,false) on bounded hang, got (%v,%v)", ttl, disabled)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("bound not applied: GetModelCacheConfig took %v", elapsed)
	}
}

// With NO explicit setter the internal default (~2s) must apply — a hanging
// reader returns around 2s, well under the test ceiling.
func TestGetModelCacheConfig_LookupTimeout_Default2s(t *testing.T) {
	s := &CacheService{}
	s.modelConfigReader = func(ctx context.Context, modelName string) (*model.ProviderModel, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	s.GetModelCacheConfig(context.Background(), "model-hang-default")
	if e := time.Since(start); e < 1500*time.Millisecond || e > 3*time.Second {
		t.Fatalf("default bound not ~2s: %v", e)
	}
}

func TestGetModelCacheConfig_SuccessAndInProcessCache(t *testing.T) {
	s := &CacheService{}
	s.modelConfigReader = func(ctx context.Context, modelName string) (*model.ProviderModel, error) {
		return &model.ProviderModel{ExtraConfig: datatypes.JSON(`{"cache_ttl_minutes":7,"cache_disabled":true}`)}, nil
	}
	s.SetModelConfigLookupTimeout(time.Second)

	ttl, disabled := s.GetModelCacheConfig(context.Background(), "model-ok")
	if ttl != 7*time.Minute || !disabled {
		t.Fatalf("expected (7m,true) from ExtraConfig, got (%v,%v)", ttl, disabled)
	}

	// Second call must be served by the 2-minute in-process modelCache: the
	// reader is not consulted again (the r78e hang path is per-model COLD
	// only).
	calls := 0
	s.modelConfigReader = func(ctx context.Context, modelName string) (*model.ProviderModel, error) {
		calls++
		return &model.ProviderModel{}, nil
	}
	s.GetModelCacheConfig(context.Background(), "model-ok")
	if calls != 0 {
		t.Fatalf("expected in-process cache hit, reader called %d times", calls)
	}
}
