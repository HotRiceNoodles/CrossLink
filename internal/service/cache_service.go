package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type CacheEntry struct {
	Model        string          `json:"model"`
	Endpoint     string          `json:"endpoint"`
	CachedAt     int64           `json:"cached_at"`
	Body         json.RawMessage `json:"body"`
	InputTokens  int             `json:"input_tokens,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
	Compressed   bool            `json:"compressed,omitempty"`
}

type CacheStats struct {
	TotalEntries  int64            `json:"total_entries"`
	EstimatedSize int64            `json:"estimated_size_bytes"`
	ByModel       []ModelCacheStat `json:"by_model"`
}

type ModelCacheStat struct {
	Model string `json:"model"`
	Count int64  `json:"count"`
}

type modelCacheEntry struct {
	ttl       time.Duration
	disabled  bool
	expiresAt time.Time
}

type CacheService struct {
	rdb           *redis.Client
	db            *gorm.DB
	enabled       atomic.Bool
	defaultTTL    atomic.Int64
	embeddingsTTL atomic.Int64
	maxBodySize   int
	modelCache    sync.Map

	statsScript *redis.Script
}

func NewCacheService(rdb *redis.Client, db *gorm.DB, cfg config.CacheConfig) *CacheService {
	s := &CacheService{
		rdb:         rdb,
		db:          db,
		maxBodySize: cfg.MaxBodySize,
	}
	s.enabled.Store(cfg.Enabled)
	s.defaultTTL.Store(int64(cfg.DefaultTTL))
	s.embeddingsTTL.Store(int64(cfg.EmbeddingsTTL))

	s.statsScript = redis.NewScript(`
		local keys = KEYS
		local models = {}
		local totalSize = 0
		local liveCount = 0
		for _, key in ipairs(keys) do
			local data = redis.call('GET', key)
			if data then
				liveCount = liveCount + 1
				totalSize = totalSize + string.len(data)
				local ok, entry = pcall(cjson.decode, data)
				if ok and type(entry) == "table" and entry.model then
					table.insert(models, entry.model)
				end
			end
		end
		return {tostring(liveCount), tostring(totalSize), unpack(models)}
	`)

	return s
}

func (s *CacheService) IsEnabled() bool              { return s.enabled.Load() }
func (s *CacheService) SetEnabled(v bool)             { s.enabled.Store(v) }
func (s *CacheService) DefaultTTL() time.Duration     { return time.Duration(s.defaultTTL.Load()) }
func (s *CacheService) SetDefaultTTL(d time.Duration) { s.defaultTTL.Store(int64(d)) }
func (s *CacheService) EmbeddingsTTL() time.Duration  { return time.Duration(s.embeddingsTTL.Load()) }
func (s *CacheService) SetEmbeddingsTTL(d time.Duration) { s.embeddingsTTL.Store(int64(d)) }
func (s *CacheService) MaxBodySize() int              { return s.maxBodySize }

func (s *CacheService) GetTTLForEndpoint(path string) time.Duration {
	if strings.Contains(path, "/embeddings") {
		return s.EmbeddingsTTL()
	}
	return s.DefaultTTL()
}

func (s *CacheService) GetModelCacheConfig(ctx context.Context, modelName string) (time.Duration, bool) {
	if cached, ok := s.modelCache.Load(modelName); ok {
		entry := cached.(*modelCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.ttl, entry.disabled
		}
		s.modelCache.Delete(modelName)
	}

	var pm model.ProviderModel
	if err := s.db.WithContext(ctx).
		Where("model_name = ? AND status = 1", modelName).
		First(&pm).Error; err != nil {
		return 0, false
	}

	result := &modelCacheEntry{
		expiresAt: time.Now().Add(2 * time.Minute),
	}

	if pm.ExtraConfig != nil {
		var cfg struct {
			CacheTTLMinutes int  `json:"cache_ttl_minutes"`
			CacheDisabled   bool `json:"cache_disabled"`
		}
		if json.Unmarshal(pm.ExtraConfig, &cfg) == nil {
			if cfg.CacheTTLMinutes > 0 {
				result.ttl = time.Duration(cfg.CacheTTLMinutes) * time.Minute
			}
			result.disabled = cfg.CacheDisabled
		}
	}

	s.modelCache.Store(modelName, result)
	return result.ttl, result.disabled
}

func (s *CacheService) Get(ctx context.Context, key string) (*CacheEntry, bool) {
	data, err := s.rdb.Get(ctx, key).Bytes()
	if err != nil || len(data) == 0 {
		return nil, false
	}

	// Try JSON first (uncompressed or legacy)
	var entry CacheEntry
	if json.Unmarshal(data, &entry) == nil {
		// Backward compat: check Body is valid JSON
		if len(entry.Body) > 0 && entry.Body[0] != '{' && entry.Body[0] != '[' && entry.Body[0] != '"' {
			return nil, false
		}
		return &entry, true
	}

	// Try gzip decompression
	gz, gzErr := gzip.NewReader(bytes.NewReader(data))
	if gzErr != nil {
		return nil, false
	}
	decompressed, readErr := io.ReadAll(gz)
	gz.Close()
	if readErr != nil {
		return nil, false
	}
	if json.Unmarshal(decompressed, &entry) != nil {
		return nil, false
	}
	if len(entry.Body) > 0 && entry.Body[0] != '{' && entry.Body[0] != '[' && entry.Body[0] != '"' {
		return nil, false
	}
	return &entry, true
}

func (s *CacheService) Set(ctx context.Context, key, model, endpoint string, body []byte, ttl time.Duration) error {
	entry := CacheEntry{
		Model:    model,
		Endpoint: endpoint,
		CachedAt: time.Now().Unix(),
		Body:     json.RawMessage(body),
	}
	// Extract token usage from response body for cache hit accounting
	var usageProbe struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &usageProbe) == nil {
		if usageProbe.Usage.PromptTokens > 0 || usageProbe.Usage.CompletionTokens > 0 {
			entry.InputTokens = usageProbe.Usage.PromptTokens
			entry.OutputTokens = usageProbe.Usage.CompletionTokens
		} else if usageProbe.Usage.InputTokens > 0 || usageProbe.Usage.OutputTokens > 0 {
			entry.InputTokens = usageProbe.Usage.InputTokens
			entry.OutputTokens = usageProbe.Usage.OutputTokens
		}
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	// Compress large entries (>4KB) to save Redis memory
	if len(body) > 4096 {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, gzErr := gz.Write(data); gzErr == nil {
			gz.Close()
			if buf.Len() < len(data) {
				data = buf.Bytes()
			}
		}
	}
	pipe := s.rdb.Pipeline()
	pipe.Set(ctx, key, data, ttl)
	pipe.SAdd(ctx, "cl:cache:idx:"+model, key)
	pipe.HIncrBy(ctx, "cl:cache:stats:counts", model, 1)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	// Set TTL on index set to prevent unbounded growth; 24h covers typical cache TTL range
	s.rdb.Expire(ctx, "cl:cache:idx:"+model, 24*time.Hour)
	return nil
}

func (s *CacheService) FlushAll(ctx context.Context) {
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, "cl:cache:*", 500).Result()
		if err != nil {
			slog.Warn("flush cache scan failed", "error", err)
			return
		}
		if len(keys) > 0 {
			if _, err := s.rdb.Del(ctx, keys...).Result(); err != nil {
				slog.Warn("flush cache del failed", "error", err, "batch", len(keys))
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	// Clean up index sets and stats
	s.rdb.Del(ctx, "cl:cache:stats:counts")
	idxCursor := uint64(0)
	for {
		keys, next, err := s.rdb.Scan(ctx, idxCursor, "cl:cache:idx:*", 500).Result()
		if err != nil {
			break
		}
		if len(keys) > 0 {
			s.rdb.Del(ctx, keys...)
		}
		if next == 0 {
			break
		}
		idxCursor = next
	}
}

func (s *CacheService) FlushByModel(ctx context.Context, modelName string) int {
	idxKey := "cl:cache:idx:" + modelName
	members, err := s.rdb.SMembers(ctx, idxKey).Result()
	if err != nil {
		slog.Warn("flush by model: failed to read index", "error", err, "model", modelName)
		return 0
	}
	if len(members) == 0 {
		return 0
	}
	deleted, err := s.rdb.Del(ctx, members...).Result()
	if err != nil {
		slog.Warn("flush by model: del failed", "error", err, "model", modelName)
	}
	s.rdb.Del(ctx, idxKey)
	s.rdb.HDel(ctx, "cl:cache:stats:counts", modelName)
	return int(deleted)
}

func (s *CacheService) Stats(ctx context.Context) (*CacheStats, error) {
	stats := &CacheStats{}
	modelCounts := make(map[string]int64)
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, "cl:cache:*", 500).Result()
		if err != nil {
			return stats, err
		}
		if len(keys) > 0 {
			result, err := s.statsScript.Run(ctx, s.rdb, keys).Slice()
			if err != nil {
				slog.Warn("cache stats script failed", "error", err)
			} else if len(result) >= 2 {
				if liveCount, err := strconv.ParseInt(toString(result[0]), 10, 64); err == nil {
					stats.TotalEntries += liveCount
				}
				if totalSize, err := strconv.ParseInt(toString(result[1]), 10, 64); err == nil {
					stats.EstimatedSize += totalSize
				}
				for i := 2; i < len(result); i++ {
					if m := toString(result[i]); m != "" {
						modelCounts[m]++
					}
				}
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	for m, count := range modelCounts {
		stats.ByModel = append(stats.ByModel, ModelCacheStat{Model: m, Count: count})
	}
	return stats, nil
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (s *CacheService) InvalidateModelCache(modelName string) {
	s.modelCache.Delete(modelName)
	slog.Info("invalidated model config cache", "model", modelName)
}

func (s *CacheService) QuickStats(ctx context.Context) *CacheStats {
	stats := &CacheStats{}
	result, err := s.rdb.HGetAll(ctx, "cl:cache:stats:counts").Result()
	if err != nil {
		return stats
	}
	var total int64
	for m, val := range result {
		count, _ := strconv.ParseInt(val, 10, 64)
		if count > 0 {
			stats.ByModel = append(stats.ByModel, ModelCacheStat{Model: m, Count: count})
			total += count
		}
	}
	stats.TotalEntries = total
	return stats
}
