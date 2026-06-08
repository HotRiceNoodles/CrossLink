package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/secret"
)

var (
	ErrTaskNotFound = errors.New("video task not found")
	ErrTaskNotReady = errors.New("video task not completed")
	ErrTaskExpired  = errors.New("video task expired")
)

const (
	videoTaskKeyPrefix  = "cl:video:task:"
	videoStatusKeyPrefix = "cl:video:status:"
	videoBilledKeyPrefix = "cl:video:billed:"
	videoTaskTTL         = 6 * time.Hour
	videoStatusTTL       = 30 * time.Minute
	videoBilledTTL       = 24 * time.Hour
)

// VideoSubmitParams holds the parameters for submitting a video task.
type VideoSubmitParams struct {
	UpstreamTaskID string
	ProviderName   string
	APIKey         string // plaintext; will be encrypted before storage
	Model          string
	OrgID          int64
	APIKeyID       int64
	InputPrice     float64
	Prompt         string
}

// VideoTaskState holds the stored task state retrieved from Redis/memory.
type VideoTaskState struct {
	UpstreamTaskID string
	ProviderName   string
	APIKey         string // decrypted plaintext
	Model          string
	OrgID          int64
	APIKeyID       int64
	InputPrice     float64
	CreatedAt      int64
}

// VideoTaskService manages video task lifecycle across Redis (with sync.Map fallback).
type VideoTaskService struct {
	rdb      *redis.Client
	encStore *secret.EncryptedDBStore
	registry *provider.Registry
	fallback sync.Map // used when rdb == nil

	usageSvc *UsageService

	mu     sync.Mutex
	cancel context.CancelFunc
}

func NewVideoTaskService(rdb *redis.Client, encStore *secret.EncryptedDBStore, registry *provider.Registry, usageSvc *UsageService) *VideoTaskService {
	s := &VideoTaskService{
		rdb:      rdb,
		encStore: encStore,
		registry: registry,
		usageSvc: usageSvc,
	}

	if rdb == nil {
		// Start background cleanup for in-memory fallback
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		go s.cleanupLoop(ctx)
	}

	return s
}

// Close stops the background cleanup goroutine.
func (s *VideoTaskService) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}

// SubmitTask generates a gateway task ID, encrypts the API key, and stores the mapping.
func (s *VideoTaskService) SubmitTask(ctx context.Context, params VideoSubmitParams) (string, error) {
	gwTaskID := "video_" + uuid.New().String()[:8] + uuid.New().String()[:8]

	createdAt := time.Now().Unix()

	if s.rdb != nil {
		if s.encStore == nil {
			return "", fmt.Errorf("encryption store not configured")
		}
		// Encrypt API key
		encKey, err := s.encStore.Encrypt(params.APIKey)
		if err != nil {
			return "", fmt.Errorf("encrypt api key: %w", err)
		}

		key := videoTaskKeyPrefix + gwTaskID
		fields := map[string]interface{}{
			"upstream_task_id": params.UpstreamTaskID,
			"provider_name":    params.ProviderName,
			"api_key_enc":      encKey,
			"org_id":           strconv.FormatInt(params.OrgID, 10),
			"api_key_id":       strconv.FormatInt(params.APIKeyID, 10),
			"input_price":      strconv.FormatFloat(params.InputPrice, 'f', -1, 64),
			"model":            params.Model,
			"created_at":       strconv.FormatInt(createdAt, 10),
			"request_prompt":   truncateString(params.Prompt, 200),
		}
		if err := s.rdb.HSet(ctx, key, fields).Err(); err != nil {
			return "", fmt.Errorf("redis hset: %w", err)
		}
		if err := s.rdb.Expire(ctx, key, videoTaskTTL).Err(); err != nil {
			return "", fmt.Errorf("redis expire: %w", err)
		}
	} else {
		// Fallback: store in memory (no encryption)
		s.fallback.Store(gwTaskID, &fallbackEntry{
			state: &VideoTaskState{
				UpstreamTaskID: params.UpstreamTaskID,
				ProviderName:   params.ProviderName,
				APIKey:         params.APIKey,
				Model:          params.Model,
				OrgID:          params.OrgID,
				APIKeyID:       params.APIKeyID,
				InputPrice:     params.InputPrice,
				CreatedAt:      createdAt,
			},
			expiresAt: time.Now().Add(videoTaskTTL),
		})
	}

	return gwTaskID, nil
}

// GetTask retrieves the task state, validates tenant isolation, queries the upstream
// provider for current status, handles billing dedup, and caches the status.
func (s *VideoTaskService) GetTask(ctx context.Context, gwTaskID string, orgID int64) (*domain.VideoTask, *VideoTaskState, error) {
	state, err := s.getStoredState(ctx, gwTaskID)
	if err != nil {
		return nil, nil, err
	}

	// Tenant isolation
	if state.OrgID != orgID {
		return nil, nil, ErrTaskNotFound
	}

	// Check status cache
	if s.rdb != nil {
		cachedStatus, err := s.rdb.Get(ctx, videoStatusKeyPrefix+gwTaskID).Result()
		if err == nil && cachedStatus != "" {
			var cached domain.VideoTask
			if json.Unmarshal([]byte(cachedStatus), &cached) == nil {
				// Check billing dedup for completed tasks from cache
				if cached.Status == "completed" || cached.Status == "failed" {
					s.ensureBilled(ctx, gwTaskID, state, &cached)
				}
				return &cached, state, nil
			}
		}
	}

	// Query upstream provider
	task, err := s.queryUpstream(ctx, state)
	if err != nil {
		return nil, nil, err
	}

	// Billing dedup on completion
	if task.Status == "completed" || task.Status == "failed" {
		s.ensureBilled(ctx, gwTaskID, state, task)
	}

	// Cache status
	s.cacheStatus(ctx, gwTaskID, task)

	return task, state, nil
}

// GetContentURL returns the upstream download URL for a completed video.
// Checks status cache first to avoid unnecessary upstream queries.
func (s *VideoTaskService) GetContentURL(ctx context.Context, gwTaskID string, orgID int64) (string, error) {
	// Fast path: check status cache for already-completed tasks
	if s.rdb != nil {
		cachedStatus, err := s.rdb.Get(ctx, videoStatusKeyPrefix+gwTaskID).Result()
		if err == nil && cachedStatus != "" {
			var cached domain.VideoTask
			if json.Unmarshal([]byte(cachedStatus), &cached) == nil {
				if cached.Status == "completed" && cached.VideoURL != "" {
					// Still need tenant isolation check
					state, stateErr := s.getStoredState(ctx, gwTaskID)
					if stateErr != nil {
						return "", stateErr
					}
					if state.OrgID != orgID {
						return "", ErrTaskNotFound
					}
					return cached.VideoURL, nil
				}
			}
		}
	}

	// Slow path: full GetTask (queries upstream if needed)
	task, _, err := s.GetTask(ctx, gwTaskID, orgID)
	if err != nil {
		return "", err
	}
	if task.Status != "completed" {
		return "", ErrTaskNotReady
	}
	if task.VideoURL == "" {
		return "", ErrTaskExpired
	}
	return task.VideoURL, nil
}

// getStoredState retrieves the stored task mapping from Redis or memory fallback.
func (s *VideoTaskService) getStoredState(ctx context.Context, gwTaskID string) (*VideoTaskState, error) {
	if s.rdb != nil {
		m, err := s.rdb.HGetAll(ctx, videoTaskKeyPrefix+gwTaskID).Result()
		if err != nil {
			return nil, fmt.Errorf("redis hgetall: %w", err)
		}
		if len(m) == 0 {
			return nil, ErrTaskNotFound
		}

		// Decrypt API key
		if s.encStore == nil {
			return nil, fmt.Errorf("encryption store not configured")
		}
		plainKey, err := s.encStore.Decrypt(m["api_key_enc"])
		if err != nil {
			slog.Warn("video task api key decryption failed", "task_id", gwTaskID, "error", err)
			return nil, ErrTaskNotFound
		}

		orgID, _ := strconv.ParseInt(m["org_id"], 10, 64)
		apiKeyID, _ := strconv.ParseInt(m["api_key_id"], 10, 64)
		inputPrice, _ := strconv.ParseFloat(m["input_price"], 64)
		createdAt, _ := strconv.ParseInt(m["created_at"], 10, 64)

		return &VideoTaskState{
			UpstreamTaskID: m["upstream_task_id"],
			ProviderName:   m["provider_name"],
			APIKey:         plainKey,
			Model:          m["model"],
			OrgID:          orgID,
			APIKeyID:       apiKeyID,
			InputPrice:     inputPrice,
			CreatedAt:      createdAt,
		}, nil
	}

	// Fallback: memory
	v, ok := s.fallback.Load(gwTaskID)
	if !ok {
		return nil, ErrTaskNotFound
	}
	entry := v.(*fallbackEntry)
	if time.Now().After(entry.expiresAt) {
		s.fallback.Delete(gwTaskID)
		return nil, ErrTaskNotFound
	}
	return entry.state, nil
}

// queryUpstream looks up the provider from the registry and queries its status.
func (s *VideoTaskService) queryUpstream(ctx context.Context, state *VideoTaskState) (*domain.VideoTask, error) {
	p, ok := s.registry.Get(state.ProviderName)
	if !ok {
		slog.Warn("video task references deleted/disabled provider", "provider", state.ProviderName, "model", state.Model)
		return nil, ErrTaskNotFound
	}

	vp, ok := p.(provider.VideoProvider)
	if !ok {
		slog.Error("video task provider does not implement VideoProvider", "provider", state.ProviderName)
		return nil, fmt.Errorf("provider %q is not a video provider", state.ProviderName)
	}

	task, err := vp.GetVideoTaskStatus(ctx, state.UpstreamTaskID, state.APIKey)
	if err != nil {
		return nil, fmt.Errorf("query upstream: %w", err)
	}

	return task, nil
}

// ensureBilled performs SETNX billing dedup. Records usage on first completion.
func (s *VideoTaskService) ensureBilled(ctx context.Context, gwTaskID string, state *VideoTaskState, task *domain.VideoTask) {
	if s.rdb == nil || s.usageSvc == nil {
		return
	}
	if task.Status != "completed" {
		return
	}

	dedupeKey := videoBilledKeyPrefix + gwTaskID
	ok, err := s.rdb.SetNX(ctx, dedupeKey, 1, videoBilledTTL).Result()
	if err != nil {
		slog.Warn("video billing dedup setnx failed", "task_id", gwTaskID, "error", err)
		return
	}
	if !ok {
		return // already billed
	}

	// Log usage
	cost := state.InputPrice
	if task.Usage != nil && task.Usage.Cost > 0 {
		cost = task.Usage.Cost
	}

	s.usageSvc.Log(context.Background(), &UsageEntry{
		RouteType:       "video",
		ModelUsed:       state.Model,
		OrgID:           state.OrgID,
		APIKeyID:        state.APIKeyID,
		StatusCode:      200,
		PrecomputedCost: cost,
	})
}

// cacheStatus writes the task status to Redis with a short TTL.
func (s *VideoTaskService) cacheStatus(ctx context.Context, gwTaskID string, task *domain.VideoTask) {
	if s.rdb == nil {
		return
	}
	data, err := json.Marshal(task)
	if err != nil {
		return
	}
	if err := s.rdb.Set(ctx, videoStatusKeyPrefix+gwTaskID, data, videoStatusTTL).Err(); err != nil {
		slog.Warn("video status cache write failed", "task_id", gwTaskID, "error", err)
	}
}

// --- Memory fallback ---

type fallbackEntry struct {
	state     *VideoTaskState
	expiresAt time.Time
}

func (s *VideoTaskService) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			s.fallback.Range(func(key, value any) bool {
				if entry, ok := value.(*fallbackEntry); ok && now.After(entry.expiresAt) {
					s.fallback.Delete(key)
				}
				return true
			})
		}
	}
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
