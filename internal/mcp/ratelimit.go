package mcp

import (
	"fmt"
	"sync"
	"time"
)

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rpm     float64
}

type tokenBucket struct {
	tokens   float64
	lastFill time.Time
}

const maxBuckets = 10000
const bucketTTL = 10 * time.Minute

func NewRateLimiter(rpm int) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*tokenBucket),
		rpm:     float64(rpm),
	}
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if len(rl.buckets) > maxBuckets {
		now := time.Now()
		for k, b := range rl.buckets {
			if now.Sub(b.lastFill) > bucketTTL {
				delete(rl.buckets, k)
			}
		}
	}

	b, ok := rl.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: rl.rpm, lastFill: time.Now()}
		rl.buckets[key] = b
	}

	// Refill tokens based on elapsed time
	elapsed := time.Since(b.lastFill)
	b.tokens += elapsed.Seconds() * (rl.rpm / 60)
	if b.tokens > rl.rpm {
		b.tokens = rl.rpm
	}
	b.lastFill = time.Now()

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (rl *RateLimiter) String() string {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return fmt.Sprintf("RateLimiter(rpm=%d, keys=%d)", int(rl.rpm), len(rl.buckets))
}
