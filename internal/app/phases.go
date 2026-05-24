package app

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/crosslink/internal/admin"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	"gorm.io/gorm"
)

// buildAuth validates admin config and seeds admin user + permissions.
func buildAuth(db *gorm.DB, cfg *config.Config) {
	admin.LoadAdminPassword(db, &cfg.Admin)
	if cfg.Admin.IsDefaultJWTSecret() {
		slog.Error("insecure JWT secret detected: change admin.jwt_secret in config or set CL_ADMIN_JWT_SECRET env var")
		os.Exit(1)
	}
	if cfg.Admin.IsJWTSecretInsecure() {
		slog.Error("JWT secret must be at least 32 characters", "length", len(cfg.Admin.JWTSecret))
		os.Exit(1)
	}
	ensureAdminUser(db, &cfg.Admin)
	syncAdminPermissions(db)
}

// SecretsBundle holds the outputs of buildSecrets.
type SecretsBundle struct {
	SecretResolver *secret.SecretResolver
	EncStore       *secret.EncryptedDBStore
	ActiveKeyPtr   *string
	CleanupCancel  context.CancelFunc
}

// buildSecrets initializes secret resolver, encryption store, and background key watcher.
func buildSecrets(db *gorm.DB, cfg *config.Config, ext *Extensions, cp crypto.CryptoProvider, rdb *redis.Client) *SecretsBundle {
	// Initialize SecretResolver
	secretResolver := secret.NewSecretResolver(cfg.SecretManager.CacheTTL)
	secretResolver.Register(secret.NewEnvSecretStore())

	// Determine active encryption key: DB (post-rotation) takes priority over config
	activeKey := cfg.SecretManager.EncryptionKey
	activeKeyPtr := &activeKey
	var dbKey model.SystemSetting
	if result := db.Where("key = ?", "encryption_key").First(&dbKey); result.Error == nil && dbKey.Value != "" {
		activeKey = dbKey.Value
	}

	var encStore *secret.EncryptedDBStore
	if activeKey != "" {
		var err error
		encStore, err = secret.NewEncryptedDBStore(activeKey, cp)
		if err != nil {
			// DB key corrupted: try fallback to config key
			if cfg.SecretManager.EncryptionKey != "" && cfg.SecretManager.EncryptionKey != activeKey {
				slog.Warn("DB encryption key invalid, falling back to config key", "error", err)
				activeKey = cfg.SecretManager.EncryptionKey
				encStore, err = secret.NewEncryptedDBStore(activeKey, cp)
			}
			if err != nil {
				slog.Error("no valid encryption key available, encrypted secrets are inaccessible", "error", err)
				os.Exit(1)
			}
		}
		secretResolver.Register(encStore)
		secretResolver.Register(encStore.AsV2())
		if result, err := secret.MigratePlaintextSecrets(db, encStore); err != nil {
			slog.Warn("secret migration encountered errors", "error", err)
		} else if len(result.Failed) > 0 {
			slog.Warn("secret migration had partial failures", "failed", result.Failed)
		}
	} else {
		slog.Warn("no encryption key configured (CL_ENCRYPTION_KEY), provider secrets stored as plaintext")
	}

	// Wire MCP encryption store
	if ext.MCPEncSetter != nil {
		ext.MCPEncSetter(encStore)
	}

	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())

	// Background key watcher: polls DB every 30s for encryption key changes
	// so multi-instance deployments stay in sync after key rotation.
	if encStore != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-cleanupCtx.Done():
					return
				case <-ticker.C:
				}
				var watchKey model.SystemSetting
				if result := db.Where("key = ?", "encryption_key").First(&watchKey); result.Error == nil && watchKey.Value != "" && watchKey.Value != *activeKeyPtr {
					if err := encStore.SetMasterKey(watchKey.Value); err == nil {
						*activeKeyPtr = watchKey.Value
						secretResolver.InvalidateCache()
						slog.Info("encryption key reloaded from DB (changed by another instance)")
					}
				}

				// Retry failed secret migrations
				if result, err := secret.MigratePlaintextSecrets(db, encStore); err != nil {
					slog.Warn("secret migration retry failed", "error", err)
				} else if len(result.Failed) > 0 {
					slog.Warn("secret migration retry had failures", "failed", result.Failed)
				} else if result.Migrated > 0 {
					slog.Info("secret migration retry succeeded", "migrated", result.Migrated)
				}
			}
		}()
	}

	return &SecretsBundle{
		SecretResolver: secretResolver,
		EncStore:       encStore,
		ActiveKeyPtr:   activeKeyPtr,
		CleanupCancel:  cleanupCancel,
	}
}
