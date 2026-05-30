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
	ensureDefaultOrganization(db)
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

func ensureDefaultOrganization(db *gorm.DB) {
	var org model.Organization
	if err := db.Where("name = ?", "default").First(&org).Error; err != nil {
		org = model.Organization{
			Name:        "default",
			DisplayName: "Default Organization",
			Status:      1,
		}
		if err := db.Create(&org).Error; err != nil {
			slog.Error("failed to create default organization", "error", err)
			os.Exit(1)
		}
		slog.Info("created default organization", "id", org.ID)
	}
	// Always run backfill — idempotent (WHERE org_id IS NULL)
	var adminUser model.User
	db.Where("role_id = (SELECT id FROM roles WHERE name = ?)", model.RoleAdmin).First(&adminUser)
	backfillOrgID(db, org.ID, adminUser.ID)
}

func backfillOrgID(db *gorm.DB, defaultOrgID int64, adminUserID int64) {
	db.Model(&model.Team{}).Where("org_id IS NULL AND deleted_at IS NULL").Update("org_id", defaultOrgID)
	db.Model(&model.APIKey{}).Where("org_id IS NULL AND deleted_at IS NULL").Update("org_id", defaultOrgID)
	db.Model(&model.Provider{}).Where("org_id IS NULL AND deleted_at IS NULL").Update("org_id", defaultOrgID)
	db.Model(&model.Role{}).Where("is_system = false AND org_id IS NULL AND deleted_at IS NULL").Update("org_id", defaultOrgID)
	// Non-admin users get org_id
	db.Model(&model.User{}).Where("org_id IS NULL AND id != ? AND deleted_at IS NULL", adminUserID).Update("org_id", defaultOrgID)
	// Register all non-admin users as org members
	var users []model.User
	db.Where("id != ? AND deleted_at IS NULL", adminUserID).Find(&users)
	for _, u := range users {
		db.FirstOrCreate(&model.OrgMember{}, model.OrgMember{OrgID: defaultOrgID, UserID: u.ID})
	}
	// Phase 2 tables
	db.Model(&model.BudgetAlert{}).Where("org_id IS NULL AND deleted_at IS NULL").Update("org_id", defaultOrgID)
	// Phase 4 small tables
	db.Exec("UPDATE insights SET org_id = ? WHERE org_id IS NULL", defaultOrgID)
	db.Exec("UPDATE optimization_actions SET org_id = ? WHERE org_id IS NULL", defaultOrgID)
	db.Exec("UPDATE budget_recommendations SET org_id = ? WHERE org_id IS NULL", defaultOrgID)
	db.Exec("UPDATE budget_requests SET org_id = ? WHERE org_id IS NULL", defaultOrgID)
	db.Exec("UPDATE budget_snapshots SET org_id = ? WHERE org_id IS NULL", defaultOrgID)
	db.Exec("UPDATE mcp_servers SET org_id = ? WHERE org_id IS NULL AND deleted_at IS NULL", defaultOrgID)
	// Phase 4 large tables: batch backfill
	backfillLargeTableOrgID(db, "usage_logs", defaultOrgID)
	backfillLargeTableOrgID(db, "mcp_tool_call_logs", defaultOrgID)
}

func backfillLargeTableOrgID(db *gorm.DB, table string, defaultOrgID int64) {
	const batchSize = 5000
	for {
		result := db.Table(table).Where("org_id IS NULL").Limit(batchSize).Update("org_id", defaultOrgID)
		if result.Error != nil {
			slog.Warn("batch backfill error", "table", table, "error", result.Error)
			return
		}
		if result.RowsAffected == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
