package provider

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var budgetScript = redis.NewScript(`
	local count = redis.call('INCR', KEYS[1])
	if count == 1 then
		redis.call('EXPIRE', KEYS[1], 2)
	end
	return count
`)

type RetryBudget struct {
	rdb  *redis.Client
	rate int
	mu   sync.Mutex
}

func NewRetryBudget(rdb *redis.Client, ratePerSecond int) *RetryBudget {
	if ratePerSecond <= 0 {
		ratePerSecond = 100
	}
	return &RetryBudget{rdb: rdb, rate: ratePerSecond}
}

func (b *RetryBudget) Allow(ctx context.Context) bool {
	key := fmt.Sprintf("retry_budget:%s", time.Now().Format("2006-01-02-15-04-05"))
	count, err := budgetScript.Run(ctx, b.rdb, []string{key}).Int64()
	if err != nil {
		return true // fail-open on Redis error
	}
	b.mu.Lock()
	rate := b.rate
	b.mu.Unlock()
	return count <= int64(rate)
}

func (b *RetryBudget) UpdateRate(rate int) {
	if rate <= 0 {
		slog.Warn("UpdateRate called with invalid rate, ignoring", "rate", rate)
		return
	}
	b.mu.Lock()
	b.rate = rate
	b.mu.Unlock()
}
