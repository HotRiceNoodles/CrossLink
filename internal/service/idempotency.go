package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type IdempotencyCache struct {
	rdb *redis.Client
	ttl time.Duration
}

const MaxIdempotencyKeyLen = 256

type CachedResponse struct {
	StatusCode int               `json:"status_code"`
	Body       json.RawMessage   `json:"body"`
	Headers    map[string]string `json:"headers,omitempty"`
}

func NewIdempotencyCache(rdb *redis.Client) *IdempotencyCache {
	return &IdempotencyCache{rdb: rdb, ttl: time.Hour}
}

func (c *IdempotencyCache) Get(ctx context.Context, apiKeyID int64, key string) (*CachedResponse, bool) {
	if len(key) == 0 || len(key) > MaxIdempotencyKeyLen {
		return nil, false
	}
	redisKey := fmt.Sprintf("idem:%d:%s", apiKeyID, key)
	val, err := c.rdb.Get(ctx, redisKey).Bytes()
	if err == redis.Nil {
		return nil, false
	}
	if err != nil {
		slog.Warn("idempotency cache get failed", "error", err)
		return nil, false
	}
	var resp CachedResponse
	if err := json.Unmarshal(val, &resp); err != nil {
		return nil, false
	}
	return &resp, true
}

func (c *IdempotencyCache) Set(ctx context.Context, apiKeyID int64, key string, resp *CachedResponse) error {
	if len(key) == 0 || len(key) > MaxIdempotencyKeyLen {
		slog.Warn("idempotency key rejected", "key_len", len(key))
		return nil
	}
	redisKey := fmt.Sprintf("idem:%d:%s", apiKeyID, key)
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, redisKey, data, c.ttl).Err()
}

// idempotencyProcessingTTL bounds how long a processing marker can live: it
// must cover the longest upstream call, and self-expires if a request crashes
// without releasing (otherwise the key would be poisoned until then).
const idempotencyProcessingTTL = 2 * time.Minute

// TryAcquire atomically claims the idempotency key for in-flight execution
// (SETNX on a processing marker). It closes the concurrent-duplicate window
// between Get (miss) and Set: without it, two requests sharing a key both saw
// "no cached response" and both executed (double upstream calls, double
// billing). Returns false when another request with the same key is in flight.
func (c *IdempotencyCache) TryAcquire(ctx context.Context, apiKeyID int64, key string) bool {
	if len(key) == 0 || len(key) > MaxIdempotencyKeyLen {
		return true // invalid keys were never protected; don't block the request
	}
	redisKey := fmt.Sprintf("idem:proc:%d:%s", apiKeyID, key)
	ok, err := c.rdb.SetNX(ctx, redisKey, 1, idempotencyProcessingTTL).Result()
	if err != nil {
		slog.Warn("idempotency acquire failed", "error", err)
		return true // fail open, matching Get's error behavior
	}
	return ok
}

// Release drops the processing marker so the key is reusable immediately
// (request failed, or finished — the result cache takes over from here).
func (c *IdempotencyCache) Release(ctx context.Context, apiKeyID int64, key string) {
	if len(key) == 0 || len(key) > MaxIdempotencyKeyLen {
		return
	}
	c.rdb.Del(ctx, fmt.Sprintf("idem:proc:%d:%s", apiKeyID, key))
}
