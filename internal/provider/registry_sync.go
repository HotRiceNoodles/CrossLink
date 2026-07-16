package provider

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const RegistrySyncChannel = "crosslink:registry:sync"

// registrySyncMessage is the payload published to the Redis Pub/Sub channel.
type registrySyncMessage struct {
	Action       string `json:"action"`
	ProviderName string `json:"provider_name"`
	Timestamp    int64  `json:"timestamp"`
	InstanceID   string `json:"instance_id"`
}

// RegistrySync keeps provider registries in sync across multiple gateway
// instances using Redis Pub/Sub. When one instance mutates a provider (create,
// update, delete), it publishes a notification so other instances reload the
// change from the database.
type RegistrySync struct {
	rdb            *redis.Client
	registry       *Registry
	db             *gorm.DB
	secretResolver *secret.SecretResolver
	instanceID     string

	ctx    context.Context
	cancel context.CancelFunc
}

// NewRegistrySync creates a new RegistrySync. The caller must invoke Start to
// begin listening for notifications and Stop to shut down.
func NewRegistrySync(rdb *redis.Client, registry *Registry, db *gorm.DB, secretResolver *secret.SecretResolver) *RegistrySync {
	return &RegistrySync{
		rdb:            rdb,
		registry:       registry,
		db:             db,
		secretResolver: secretResolver,
		instanceID:     uuid.New().String(),
	}
}

// NotifyReload publishes a "reload" message so other instances re-read the
// provider from the database and register it.
func (s *RegistrySync) NotifyReload(providerName string) {
	s.publish(registrySyncMessage{
		Action:       "reload",
		ProviderName: providerName,
		Timestamp:    time.Now().Unix(),
		InstanceID:   s.instanceID,
	})
}

// NotifyRemove publishes a "remove" message so other instances remove the
// provider from their in-memory registry.
func (s *RegistrySync) NotifyRemove(providerName string) {
	s.publish(registrySyncMessage{
		Action:       "remove",
		ProviderName: providerName,
		Timestamp:    time.Now().Unix(),
		InstanceID:   s.instanceID,
	})
}

func (s *RegistrySync) publish(msg registrySyncMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("registry_sync: marshal message failed", "error", err)
		return
	}
	if err := s.rdb.Publish(context.Background(), RegistrySyncChannel, data).Err(); err != nil {
		slog.Error("registry_sync: publish failed", "channel", RegistrySyncChannel, "error", err)
	}
}

// Start subscribes to the sync channel and processes incoming messages.
// It blocks until ctx is cancelled or Stop is called.
func (s *RegistrySync) Start(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	defer s.cancel()

	sub := s.rdb.Subscribe(s.ctx, RegistrySyncChannel)
	defer sub.Close()

	ch := sub.Channel()
	slog.Info("registry_sync: subscribed", "channel", RegistrySyncChannel, "instance_id", s.instanceID)

	for {
		select {
		case <-s.ctx.Done():
			slog.Info("registry_sync: stopped")
			return
		case msg, ok := <-ch:
			if !ok {
				slog.Info("registry_sync: channel closed, stopping")
				return
			}
			s.handleMessage(msg)
		}
	}
}

// Stop cancels the subscription goroutine.
func (s *RegistrySync) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *RegistrySync) handleMessage(msg *redis.Message) {
	var m registrySyncMessage
	if err := json.Unmarshal([]byte(msg.Payload), &m); err != nil {
		slog.Error("registry_sync: unmarshal failed", "error", err)
		return
	}

	// Skip messages from this instance.
	if m.InstanceID == s.instanceID {
		return
	}

	switch m.Action {
	case "reload":
		s.reloadProvider(m.ProviderName)
	case "remove":
		s.registry.Remove(m.ProviderName)
		slog.Info("registry_sync: removed provider", "name", m.ProviderName, "source_instance", m.InstanceID)
	default:
		slog.Warn("registry_sync: unknown action", "action", m.Action)
	}
}

// reloadProvider reads the provider from the database by name and registers it.
// If the provider is disabled or not found, it is removed from the registry.
func (s *RegistrySync) reloadProvider(name string) {
	var p model.Provider
	if err := s.db.Where("name = ?", name).First(&p).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			s.registry.Remove(name)
			slog.Info("registry_sync: removed provider (not found in DB)", "name", name)
			return
		}
		slog.Error("registry_sync: query provider failed", "name", name, "error", err)
		return
	}

	if p.Status != 1 {
		s.registry.Remove(name)
		slog.Info("registry_sync: removed provider (disabled)", "name", name)
		return
	}

	// Resolve secrets — same pattern as RegisterProvidersFromDB.
	cp := p
	if s.secretResolver != nil {
		resolvedKey, err := s.secretResolver.Resolve(context.Background(), cp.APIKey)
		if err != nil {
			slog.Error("registry_sync: resolve api_key failed", "name", name, "error", err)
			return
		}
		cp.APIKey = resolvedKey

		if len(cp.ExtraConfig) > 0 {
			var extraMap map[string]any
			if json.Unmarshal(cp.ExtraConfig, &extraMap) == nil {
				if err := s.secretResolver.ResolveExtraConfigSecrets(context.Background(), extraMap); err != nil {
					slog.Error("registry_sync: resolve extra_config failed", "name", name, "error", err)
					return
				}
				cp.ExtraConfig, _ = json.Marshal(extraMap)
			}
		}
	}

	prov, err := NewFromModel(&cp, 300*time.Second)
	if err != nil {
		slog.Error("registry_sync: create provider failed", "name", name, "adapter_type", p.AdapterType, "error", err)
		return
	}
	// VCR recording: wrap when record=true. store=nil → activeStore() reads
	// globalFixtureStore dynamically (same pattern as MockProvider).
	if IsRecordEnabled(cp.ExtraConfig) {
		prov = NewRecordingProvider(prov, name, nil)
		slog.Info("registry_sync: recording enabled", "name", name)
	}

	s.registry.Register(name, prov)
	slog.Info("registry_sync: reloaded provider", "name", name, "adapter_type", p.AdapterType)
}
