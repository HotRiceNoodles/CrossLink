package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type ActiveRequestTracker struct {
	rdb *redis.Client
}

// ProviderLoadTracker is the interface handlers use to track active requests.
type ProviderLoadTracker interface {
	Incr(ctx context.Context, providerName string)
	Decr(ctx context.Context, providerName string)
}

func NewActiveRequestTracker(rdb *redis.Client) *ActiveRequestTracker {
	return &ActiveRequestTracker{rdb: rdb}
}

// incrExpireActiveScript atomically increments the active request counter and sets TTL.
var incrExpireActiveScript = redis.NewScript(`
	local count = redis.call('INCR', KEYS[1])
	if count == 1 then
		redis.call('EXPIRE', KEYS[1], ARGV[1])
	end
	return count
`)

// decrScript atomically decrements the counter; deletes the key if it reaches 0 or less.
var decrScript = redis.NewScript(`
	local val = redis.call('GET', KEYS[1])
	if not val or tonumber(val) <= 1 then
		return redis.call('DEL', KEYS[1])
	end
	return redis.call('DECR', KEYS[1])
`)

func (t *ActiveRequestTracker) Incr(ctx context.Context, providerName string) {
	if t.rdb == nil {
		return
	}
	key := "active:" + providerName
	if _, err := incrExpireActiveScript.Run(ctx, t.rdb, []string{key}, int(30*time.Minute/time.Second)).Result(); err != nil {
		slog.Warn("active incr failed", "provider", providerName, "error", err)
	}
}

func (t *ActiveRequestTracker) Decr(ctx context.Context, providerName string) {
	if t.rdb == nil {
		return
	}
	decrScript.Run(ctx, t.rdb, []string{"active:" + providerName})
}

func (t *ActiveRequestTracker) Get(ctx context.Context, providerName string) int64 {
	if t.rdb == nil {
		return 0
	}
	val, err := t.rdb.Get(ctx, "active:"+providerName).Int64()
	if err != nil {
		return 0
	}
	if val < 0 {
		return 0
	}
	return val
}
