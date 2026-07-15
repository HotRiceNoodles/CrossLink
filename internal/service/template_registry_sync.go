package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// TemplateSyncChannel is the Redis Pub/Sub channel for cross-instance template
// cache invalidation.
const TemplateSyncChannel = "crosslink:templates:sync"

type templateSyncMessage struct {
	Action    string `json:"action"`     // "reload" (invalidate) | "remove"
	Name      string `json:"name"`
	Timestamp int64  `json:"timestamp"`
	Instance  string `json:"instance_id"`
}

// TemplateRegistrySync keeps TemplateRegistry caches consistent across gateway
// instances via Redis Pub/Sub. On any template CRUD, one instance publishes a
// notification; all instances (including the originator via its own invalidate)
// drop the cached entry so the next Get re-reads the DB.
//
// This mirrors provider.RegistrySync but is simpler: templates are plain data
// (no secret resolution / provider construction), so "reload" just invalidates
// the cache entry rather than rebuilding an object.
type TemplateRegistrySync struct {
	rdb        *redis.Client
	registry   *TemplateRegistry
	db         *gorm.DB
	instanceID string

	ctx    context.Context
	cancel context.CancelFunc
}

func NewTemplateRegistrySync(rdb *redis.Client, registry *TemplateRegistry, db *gorm.DB) *TemplateRegistrySync {
	return &TemplateRegistrySync{
		rdb:        rdb,
		registry:   registry,
		db:         db,
		instanceID: uuid.New().String(),
	}
}

// NotifyReload publishes a reload (invalidate) for a template name. The originator
// also invalidates locally so its own cache drops immediately.
func (s *TemplateRegistrySync) NotifyReload(name string) {
	s.registry.Invalidate(name)
	if s.rdb == nil {
		return
	}
	s.publish(templateSyncMessage{Action: "reload", Name: name, Timestamp: time.Now().Unix(), Instance: s.instanceID})
}

// NotifyRemove publishes a remove for a template name (soft-deleted).
func (s *TemplateRegistrySync) NotifyRemove(name string) {
	s.registry.Invalidate(name)
	if s.rdb == nil {
		return
	}
	s.publish(templateSyncMessage{Action: "remove", Name: name, Timestamp: time.Now().Unix(), Instance: s.instanceID})
}

func (s *TemplateRegistrySync) publish(msg templateSyncMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("template_sync: marshal failed", "error", err)
		return
	}
	if err := s.rdb.Publish(context.Background(), TemplateSyncChannel, data).Err(); err != nil {
		slog.Error("template_sync: publish failed", "error", err)
	}
}

// Start subscribes to the sync channel. Blocks until ctx cancelled or Stop called.
func (s *TemplateRegistrySync) Start(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	defer s.cancel()
	if s.rdb == nil {
		return
	}
	sub := s.rdb.Subscribe(s.ctx, TemplateSyncChannel)
	defer sub.Close()
	ch := sub.Channel()
	slog.Info("template_sync: subscribed", "channel", TemplateSyncChannel, "instance_id", s.instanceID)
	for {
		select {
		case <-s.ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			s.handleMessage(msg)
		}
	}
}

func (s *TemplateRegistrySync) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *TemplateRegistrySync) handleMessage(msg *redis.Message) {
	var m templateSyncMessage
	if err := json.Unmarshal([]byte(msg.Payload), &m); err != nil {
		slog.Error("template_sync: unmarshal failed", "error", err)
		return
	}
	if m.Instance == s.instanceID {
		return // skip own broadcast
	}
	switch m.Action {
	case "reload", "remove":
		s.registry.Invalidate(m.Name)
		slog.Info("template_sync: invalidated cache", "name", m.Name, "action", m.Action, "source", m.Instance)
	default:
		slog.Warn("template_sync: unknown action", "action", m.Action)
	}
}
