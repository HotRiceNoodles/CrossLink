package service

import (
	"context"
	"time"
)

// CacheServiceInterface decouples middleware from CacheService implementation.
// Community passes nil (middleware short-circuits with c.Next()).
// Commercial injects real CacheService.
type CacheServiceInterface interface {
	IsEnabled() bool
	MaxBodySize() int
	GetModelCacheConfig(ctx context.Context, model string) (ttl time.Duration, disabled bool)
	GetTTLForEndpoint(path string) time.Duration
	Get(ctx context.Context, key string) (*CacheEntry, bool)
	Set(ctx context.Context, key, model, endpoint string, body []byte, ttl time.Duration) error
}
