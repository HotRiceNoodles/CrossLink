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
	// Phase-1 effort replay binding (2026-11-25 checklist §6.2): what was
	// asked (endpoint + request fingerprint) and what executed (provider/
	// model). The replay path verifies the CURRENT policy of the ORIGINAL
	// target — not just "some candidate is still compatible" — so a result
	// produced by a since-deauthorized route is refused instead of re-served.
	// Zero values = legacy entry written before the binding existed; callers
	// apply the conservative full re-check for those.
	Endpoint     string `json:"endpoint,omitempty"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	ExecProvider string `json:"exec_provider,omitempty"`
	ExecModel    string `json:"exec_model,omitempty"`
}

// IdemVerdict is the outcome of a verified idempotency read.
type IdemVerdict string

const (
	IdemMiss     IdemVerdict = "miss"
	IdemOK       IdemVerdict = "ok"
	IdemConflict IdemVerdict = "conflict"
)

// IdemBinding carries the replay-binding fields into SetWithBinding.
type IdemBinding struct {
	Endpoint     string
	Fingerprint  string
	ExecProvider string
	ExecModel    string
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

// GetVerified is the SHARED read entry (checklist §6.2): beside the lookup it
// enforces endpoint + request-fingerprint matching, because one API key's
// idempotency key space (`idem:<apiKeyID>:<key>`) is shared across
// /v1/messages, /v1/chat/completions and the video endpoints. A verdict of
// IdemConflict carries a client-facing reason; zero-upstream-call guarantees
// are the caller's (it must reply, never re-execute, on conflict).
func (c *IdempotencyCache) GetVerified(ctx context.Context, apiKeyID int64, key, endpoint, fingerprint string) (*CachedResponse, IdemVerdict, string) {
	resp, ok := c.Get(ctx, apiKeyID, key)
	if !ok {
		return nil, IdemMiss, ""
	}
	if resp.Endpoint != "" && resp.Endpoint != endpoint {
		return nil, IdemConflict, "idempotency key was created on a different endpoint; use a unique key per endpoint"
	}
	if resp.Fingerprint != "" && resp.Fingerprint != fingerprint {
		return nil, IdemConflict, "idempotency key was used with a different request body; use a unique key per request"
	}
	return resp, IdemOK, ""
}

// SetWithBinding stores the response together with its replay binding.
func (c *IdempotencyCache) SetWithBinding(ctx context.Context, apiKeyID int64, key string, resp *CachedResponse, b IdemBinding) error {
	resp.Endpoint = b.Endpoint
	resp.Fingerprint = b.Fingerprint
	resp.ExecProvider = b.ExecProvider
	resp.ExecModel = b.ExecModel
	return c.Set(ctx, apiKeyID, key, resp)
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
