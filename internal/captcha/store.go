package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore implements Store backed by Redis. Each challenge is JSON-encoded
// under keyPrefix+id with a TTL. Load returns (zero, false, nil) for missing
// or expired keys.
type RedisStore struct {
	rdb       *redis.Client
	keyPrefix string
}

func NewRedisStore(rdb *redis.Client, keyPrefix string) *RedisStore {
	return &RedisStore{rdb: rdb, keyPrefix: keyPrefix}
}

func (s *RedisStore) key(id string) string { return s.keyPrefix + id }

func (s *RedisStore) Save(ctx context.Context, id string, c StoredChallenge, ttl time.Duration) error {
	if s.rdb == nil {
		return fmt.Errorf("captcha store: redis client is nil")
	}
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("captcha store: marshal: %w", err)
	}
	return s.rdb.Set(ctx, s.key(id), b, ttl).Err()
}

func (s *RedisStore) Load(ctx context.Context, id string) (StoredChallenge, bool, error) {
	var out StoredChallenge
	if s.rdb == nil {
		return out, false, fmt.Errorf("captcha store: redis client is nil")
	}
	b, err := s.rdb.Get(ctx, s.key(id)).Bytes()
	if err == redis.Nil {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, false, fmt.Errorf("captcha store: unmarshal: %w", err)
	}
	return out, true, nil
}

func (s *RedisStore) Delete(ctx context.Context, id string) error {
	if s.rdb == nil {
		return fmt.Errorf("captcha store: redis client is nil")
	}
	return s.rdb.Del(ctx, s.key(id)).Err()
}
