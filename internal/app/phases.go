package app

import (
	"context"
	"errors"
	"log"
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
	if cfg.Admin.IsDefaultPassword() {
		slog.Error("insecure default admin password detected: change admin.password in config or set CL_ADMIN_PASSWORD env var")
		os.Exit(1)
	}
	ensureAdminUser(db, &cfg.Admin)
	ensureDefaultOrganization(db)
	syncAdminPermissions(db)
	syncSystemRolePermissions(db)
}

// systemRolePermissions defines the minimum permissions each non-admin system role should have.
// When new ValidActions are added that should be auto-assigned to a system role, add them here.
var systemRolePermissions = map[string][]string{
	model.RoleMember: {
		"system:password",
		"license:view",
		"mcp:list",
		"mcp:view",
	},
	model.RoleViewer: {
		"license:view",
		"mcp:list",
		"mcp:view",
	},
}

func syncSystemRolePermissions(db *gorm.DB) {
	for roleName, requiredActions := range systemRolePermissions {
		var role model.Role
		if err := db.Where("name = ? AND is_system = ?", roleName, true).First(&role).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			log.Printf("[role-sync] error finding role %s: %v", roleName, err)
			continue
		}

		var existing []model.RolePermission
		db.Where("role_id = ?", role.ID).Find(&existing)
		existingSet := make(map[string]bool, len(existing))
		for _, p := range existing {
			existingSet[p.Action] = true
		}

		for _, action := range requiredActions {
			if !existingSet[action] {
				db.Create(&model.RolePermission{RoleID: role.ID, Action: action})
			}
		}
	}
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

	// Resolve active encryption key + construct store via the shared helper.
	encStore, activeKey, err := secret.InitActiveEncryption(db, cfg.SecretManager.EncryptionKey, cp)
	if err != nil {
		slog.Error("no valid encryption key available, encrypted secrets are inaccessible", "error", err)
		os.Exit(1)
	}
	activeKeyPtr := &activeKey

	// registerEnc wires an encryption store into the resolver and the MCP
	// encryption hook. Used both at startup and by the watcher's lazy
	// activation below.
	registerEnc := func(store *secret.EncryptedDBStore) {
		secretResolver.Register(store)
		secretResolver.Register(store.AsV2())
		if ext.MCPEncSetter != nil {
			ext.MCPEncSetter(store)
		}
	}

	if encStore != nil {
		registerEnc(encStore)
		if result, err := secret.MigratePlaintextSecrets(db, encStore); err != nil {
			slog.Warn("secret migration encountered errors", "error", err)
		} else if len(result.Failed) > 0 {
			slog.Warn("secret migration had partial failures", "failed", result.Failed)
		}
	} else {
		slog.Warn("no encryption key configured (CL_ENCRYPTION_KEY), provider secrets stored as plaintext")
	}

	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())

	// Background key watcher: polls DB every 30s. Two jobs:
	//
	// 1. Hot-reload when another instance rotates the key (multi-instance sync).
	// 2. Lazy activation: the process booted keyless (fresh install, exactly
	//    what the setup wizard targets) and the wizard wrote the
	//    encryption_key row — construct the store, register it, and encrypt
	//    existing plaintext secrets without a restart.
	//
	// The watcher therefore runs unconditionally. Known limitation: handlers
	// constructed with SecretsBundle.EncStore == nil keep their nil store
	// until restart, so newly written provider secrets pass through plaintext
	// briefly before a later tick's MigratePlaintextSecrets encrypts them
	// (≤ interval). The setup wizard surfaces this as restart_recommended.
	go runEncryptionWatcher(cleanupCtx, db, secretResolver, encStore, activeKeyPtr, cp, ext, registerEnc, 30*time.Second)

	return &SecretsBundle{
		SecretResolver: secretResolver,
		EncStore:       encStore,
		ActiveKeyPtr:   activeKeyPtr,
		CleanupCancel:  cleanupCancel,
	}
}

// runEncryptionWatcher is the background loop behind buildSecrets. encStore
// is the startup snapshot; the loop owns a local copy — lazy activation
// here never updates SecretsBundle.EncStore (handlers built earlier keep
// their nil store until restart; late plaintext is encrypted by the
// migration pass within one interval).
func runEncryptionWatcher(ctx context.Context, db *gorm.DB, secretResolver *secret.SecretResolver, encStore *secret.EncryptedDBStore, activeKeyPtr *string, cp crypto.CryptoProvider, ext *Extensions, registerEnc func(*secret.EncryptedDBStore), interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	warnedInvalidKey := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var watchKey model.SystemSetting
		if result := db.Where("key = ?", "encryption_key").First(&watchKey); result.Error == nil && watchKey.Value != "" {
			switch {
			case encStore != nil && watchKey.Value != *activeKeyPtr:
				if err := encStore.SetMasterKey(watchKey.Value); err == nil {
					*activeKeyPtr = watchKey.Value
					secretResolver.InvalidateCache()
					slog.Info("encryption key reloaded from DB (changed by another instance)")
				}
			case encStore == nil && watchKey.Value != *activeKeyPtr:
				store, err := secret.NewEncryptedDBStore(watchKey.Value, cp)
				if err != nil {
					if !warnedInvalidKey {
						slog.Warn("encryption_key row in DB is invalid, keeping plaintext mode", "error", err)
						warnedInvalidKey = true
					}
				} else {
					encStore = store
					*activeKeyPtr = watchKey.Value
					registerEnc(store)
					slog.Info("encryption key activated from DB (written by setup wizard) — provider secrets now stored encrypted")
				}
			}
		}

		// Retry failed secret migrations / encrypt late-arriving plaintext.
		if encStore != nil {
			if result, err := secret.MigratePlaintextSecrets(db, encStore); err != nil {
				slog.Warn("secret migration retry failed", "error", err)
			} else if len(result.Failed) > 0 {
				slog.Warn("secret migration retry had failures", "failed", result.Failed)
			} else if result.Migrated > 0 {
				slog.Info("secret migration retry succeeded", "migrated", result.Migrated)
			}
		}
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
