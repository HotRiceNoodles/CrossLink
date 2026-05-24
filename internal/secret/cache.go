package secret

import (
	"sync"
	"time"
)

const defaultMaxEntries = 1024

type cachedSecret struct {
	value     string
	expiresAt time.Time
}

type secretCache struct {
	mu         sync.RWMutex
	items      map[string]*cachedSecret
	ttl        time.Duration
	maxEntries int
}

func newSecretCache(ttl time.Duration) *secretCache {
	return &secretCache{
		items:      make(map[string]*cachedSecret),
		ttl:        ttl,
		maxEntries: defaultMaxEntries,
	}
}

func (c *secretCache) Get(key string) (string, bool) {
	c.mu.RLock()
	item, ok := c.items[key]
	if !ok {
		c.mu.RUnlock()
		return "", false
	}
	if time.Now().After(item.expiresAt) {
		c.mu.RUnlock()
		// Lazily evict expired entry — re-check under write lock to avoid TOCTOU
		c.mu.Lock()
		if cur, ok := c.items[key]; ok && time.Now().After(cur.expiresAt) {
			delete(c.items, key)
		}
		c.mu.Unlock()
		return "", false
	}
	c.mu.RUnlock()
	return item.value, true
}

func (c *secretCache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Evict expired entries if at capacity
	if len(c.items) >= c.maxEntries {
		now := time.Now()
		for k, v := range c.items {
			if now.After(v.expiresAt) {
				delete(c.items, k)
			}
		}
	}
	// If still at capacity after eviction, remove one arbitrary entry
	if len(c.items) >= c.maxEntries {
		for k := range c.items {
			delete(c.items, k)
			break
		}
	}
	c.items[key] = &cachedSecret{
		value:     value,
		expiresAt: time.Now().Add(c.ttl),
	}
}

func (c *secretCache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

func (c *secretCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*cachedSecret)
}
