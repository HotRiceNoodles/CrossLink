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
