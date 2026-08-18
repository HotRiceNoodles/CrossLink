package service

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// CalibrationService tracks per-model-family estimation bias
// (actual input_tokens / estimated tokens) as an EMA and periodically
// flushes to Redis. Estimation fallback values must NEVER be fed in
// (design §5): callers only Observe with real upstream usage.
type CalibrationService struct {
	mu       sync.Mutex
	ema      map[string]float64
	rdb      *redis.Client
	stopCh   chan struct{}
	stopOnce sync.Once
}

const calibrationAlpha = 0.1
const calibrationRedisKey = "cl:token-calibration"

func NewCalibrationService(rdb *redis.Client) *CalibrationService {
	c := &CalibrationService{ema: map[string]float64{}, rdb: rdb, stopCh: make(chan struct{})}
	if rdb != nil {
		c.load(context.Background())
		go c.flushLoop()
	}
	return c
}

// ModelFamily maps a model name to its estimation family (prefix match,
// case-insensitive — provider models carry vendor casing like GLM-4.7-Flash).
func ModelFamily(modelName string) string {
	lower := strings.ToLower(modelName)
	for _, p := range []string{"gpt", "o1", "o3", "claude", "deepseek", "qwen", "glm", "minimax", "gemini"} {
		if strings.HasPrefix(lower, p) {
			return p
		}
	}
	return "default"
}

// Observe records one real-usage ratio. actual/estimated <= 0 are skipped.
func (c *CalibrationService) Observe(modelName string, actual, estimated int) {
	if actual <= 0 || estimated <= 0 {
		return
	}
	family := ModelFamily(modelName)
	ratio := float64(actual) / float64(estimated)
	c.mu.Lock()
	old, ok := c.ema[family]
	if !ok {
		c.ema[family] = ratio
	} else {
		c.ema[family] = calibrationAlpha*ratio + (1-calibrationAlpha)*old
	}
	c.mu.Unlock()
}

// Factor returns the calibration multiplier for a family (1.0 if unseen).
func (c *CalibrationService) Factor(modelName string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.ema[ModelFamily(modelName)]; ok && f > 0 {
		return f
	}
	return 1.0
}

func (c *CalibrationService) load(ctx context.Context) {
	m, err := c.rdb.HGetAll(ctx, calibrationRedisKey).Result()
	if err != nil {
		return // memory-only fallback is acceptable (design §5)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range m {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			c.ema[k] = f
		}
	}
}

// Stop terminates the flush loop and performs a final flush. Safe to call
// multiple times (idempotent).
func (c *CalibrationService) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *CalibrationService) flushLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			c.flush()
			return
		case <-t.C:
			c.flush()
		}
	}
}

func (c *CalibrationService) flush() {
	c.mu.Lock()
	m := make(map[string]any, len(c.ema))
	for k, v := range c.ema {
		m[k] = strconv.FormatFloat(v, 'f', 4, 64)
	}
	c.mu.Unlock()
	if len(m) == 0 {
		return
	}
	if err := c.rdb.HSet(context.Background(), calibrationRedisKey, m).Err(); err != nil {
		slog.Warn("calibration flush failed", "error", err)
	}
}
