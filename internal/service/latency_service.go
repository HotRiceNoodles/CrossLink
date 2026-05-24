package service

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"
)

type LatencyService struct {
	rdb *redis.Client
}

func NewLatencyService(rdb *redis.Client) *LatencyService {
	return &LatencyService{rdb: rdb}
}

var emaScript = redis.NewScript(`
	local old = tonumber(redis.call('GET', KEYS[1])) or 0
	local new = old * 0.7 + tonumber(ARGV[1]) * 0.3
	redis.call('SET', KEYS[1], new)
	redis.call('EXPIRE', KEYS[1], 3600)
	return new
`)

func (s *LatencyService) RecordLatency(ctx context.Context, providerName string, latencyMs int64) {
	key := "latency:" + providerName
	cmd := emaScript.Run(ctx, s.rdb, []string{key}, latencyMs)
	if err := cmd.Err(); err != nil {
		slog.Warn("record latency failed", "provider", providerName, "error", err)
	}
}

func (s *LatencyService) GetAvgLatency(ctx context.Context, providerName string) float64 {
	key := "latency:" + providerName
	val, err := s.rdb.Get(ctx, key).Float64()
	if err != nil {
		return 0
	}
	return val
}
