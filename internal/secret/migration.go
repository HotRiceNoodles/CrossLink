package secret

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// MigrationResult holds the outcome of a plaintext-to-encrypted migration.
type MigrationResult struct {
	Migrated int
	Failed   []string
}

// MigratePlaintextSecrets scans providers table and encrypts any plaintext api_key
// and extra_config sensitive fields. Best-effort: individual failures are logged
// and recorded in the result, but do not abort the overall migration.
func MigratePlaintextSecrets(db *gorm.DB, encStore *EncryptedDBStore) (*MigrationResult, error) {
	if encStore == nil {
		return &MigrationResult{}, nil
	}

	var providers []struct {
		ID          int64  `gorm:"primaryKey"`
		Name        string `gorm:"column:name"`
		APIKey      string `gorm:"column:api_key"`
		ExtraConfig []byte `gorm:"column:extra_config"`
	}
	if err := db.Table("providers").Find(&providers).Error; err != nil {
		return nil, err
	}

	result := &MigrationResult{}
	for _, p := range providers {
		tx := db.Begin()
		if tx.Error != nil {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: begin tx: %v", p.Name, tx.Error))
			continue
		}
		needsUpdate := false

		// Encrypt api_key if it's plaintext
		if p.APIKey != "" && !IsReference(p.APIKey) && !encStore.IsEncrypted(p.APIKey) {
			encrypted, err := encStore.Encrypt(p.APIKey)
			if err != nil {
				tx.Rollback()
				result.Failed = append(result.Failed, fmt.Sprintf("%s: encrypt api_key: %v", p.Name, err))
				continue
			}
			if err := tx.Table("providers").Where("id = ?", p.ID).Update("api_key", encrypted).Error; err != nil {
				tx.Rollback()
				result.Failed = append(result.Failed, fmt.Sprintf("%s: update api_key: %v", p.Name, err))
				continue
			}
			needsUpdate = true
		}

		// Encrypt sensitive fields in extra_config
		if len(p.ExtraConfig) > 0 {
			var config map[string]any
			if json.Unmarshal(p.ExtraConfig, &config) != nil {
				tx.Rollback()
				result.Failed = append(result.Failed, fmt.Sprintf("%s: invalid extra_config JSON", p.Name))
				continue
			}
			var changed bool
			for k, v := range config {
				if !IsSensitiveField(k) {
					continue
				}
				strVal, ok := v.(string)
				if !ok || strVal == "" || IsReference(strVal) || encStore.IsEncrypted(strVal) {
					continue
				}
				encrypted, err := encStore.Encrypt(strVal)
				if err != nil {
					tx.Rollback()
					result.Failed = append(result.Failed, fmt.Sprintf("%s: encrypt %s: %v", p.Name, k, err))
					continue
				}
				config[k] = encrypted
				changed = true
			}
			if changed {
				data, err := json.Marshal(config)
				if err != nil {
					tx.Rollback()
					result.Failed = append(result.Failed, fmt.Sprintf("%s: marshal extra_config: %v", p.Name, err))
					continue
				}
				if err := tx.Table("providers").Where("id = ?", p.ID).Update("extra_config", data).Error; err != nil {
					tx.Rollback()
					result.Failed = append(result.Failed, fmt.Sprintf("%s: update extra_config: %v", p.Name, err))
					continue
				}
				needsUpdate = true
			}
		}

		if needsUpdate {
			if err := tx.Commit().Error; err != nil {
				result.Failed = append(result.Failed, fmt.Sprintf("%s: commit: %v", p.Name, err))
				continue
			}
			result.Migrated++
			slog.Info("migrated provider secrets", "provider", p.Name)
		} else {
			tx.Rollback()
		}
	}

	if result.Migrated > 0 {
		slog.Info("secret migration completed", "migrated_count", result.Migrated)
	}
	return result, nil
}

const v2MigrationLockKey = "gm:migration:secret"
const v2MigrationLockTTL = 5 * time.Minute

// MigrateV1ToV2 re-encrypts any v1 ("enc://") secrets to v2 ("enc2://") format.
// Uses a Redis distributed lock for multi-instance safety.
// If Redis is nil, runs without locking (single-instance mode).
// Idempotent: entries already in v2 format are skipped.
func MigrateV1ToV2(ctx context.Context, db *gorm.DB, encStore *EncryptedDBStore, rdb *redis.Client) (*MigrationResult, error) {
	if encStore == nil {
		return &MigrationResult{}, nil
	}

	// Try to acquire distributed lock
	locked := false
	if rdb != nil {
		ok, err := rdb.SetNX(ctx, v2MigrationLockKey, "1", v2MigrationLockTTL).Result()
		if err != nil {
			slog.Warn("secret v1→v2 migration: Redis lock failed, skipping", "error", err)
			return &MigrationResult{}, nil
		}
		if !ok {
			slog.Info("secret v1→v2 migration: another instance holds the lock, skipping")
			return &MigrationResult{}, nil
		}
		locked = true
		defer func() {
			rdb.Del(ctx, v2MigrationLockKey)
		}()
	}

	result := &MigrationResult{}

	var providers []struct {
		ID          int64  `gorm:"primaryKey"`
		Name        string `gorm:"column:name"`
		APIKey      string `gorm:"column:api_key"`
		ExtraConfig []byte `gorm:"column:extra_config"`
	}
	if err := db.Table("providers").Find(&providers).Error; err != nil {
		return nil, fmt.Errorf("scan providers: %w", err)
	}

	for _, p := range providers {
		tx := db.Begin()
		if tx.Error != nil {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: begin tx: %v", p.Name, tx.Error))
			continue
		}
		needsUpdate := false

		// Migrate api_key if it's v1 format
		if strings.HasPrefix(p.APIKey, "enc://") {
			plaintext, err := encStore.Decrypt(p.APIKey)
			if err != nil {
				tx.Rollback()
				result.Failed = append(result.Failed, fmt.Sprintf("%s: decrypt api_key: %v", p.Name, err))
				continue
			}
			reEncrypted, err := encStore.Encrypt(plaintext)
			if err != nil {
				tx.Rollback()
				result.Failed = append(result.Failed, fmt.Sprintf("%s: re-encrypt api_key: %v", p.Name, err))
				continue
			}
			if err := tx.Table("providers").Where("id = ?", p.ID).Update("api_key", reEncrypted).Error; err != nil {
				tx.Rollback()
				result.Failed = append(result.Failed, fmt.Sprintf("%s: update api_key: %v", p.Name, err))
				continue
			}
			needsUpdate = true
		}

		// Migrate sensitive fields in extra_config
		if len(p.ExtraConfig) > 0 {
			var config map[string]any
			if json.Unmarshal(p.ExtraConfig, &config) != nil {
				tx.Rollback()
				continue
			}
			var changed bool
			for k, v := range config {
				if !IsSensitiveField(k) {
					continue
				}
				strVal, ok := v.(string)
				if !ok || !strings.HasPrefix(strVal, "enc://") {
					continue
				}
				plaintext, err := encStore.Decrypt(strVal)
				if err != nil {
					continue
				}
				reEncrypted, err := encStore.Encrypt(plaintext)
				if err != nil {
					continue
				}
				config[k] = reEncrypted
				changed = true
			}
			if changed {
				data, err := json.Marshal(config)
				if err != nil {
					tx.Rollback()
					result.Failed = append(result.Failed, fmt.Sprintf("%s: marshal extra_config: %v", p.Name, err))
					continue
				}
				if err := tx.Table("providers").Where("id = ?", p.ID).Update("extra_config", data).Error; err != nil {
					tx.Rollback()
					result.Failed = append(result.Failed, fmt.Sprintf("%s: update extra_config: %v", p.Name, err))
					continue
				}
				needsUpdate = true
			}
		}

		if needsUpdate {
			if err := tx.Commit().Error; err != nil {
				result.Failed = append(result.Failed, fmt.Sprintf("%s: commit: %v", p.Name, err))
				continue
			}
			result.Migrated++
			slog.Info("migrated provider secret v1→v2", "provider", p.Name)
		} else {
			tx.Rollback()
		}
	}

	if locked {
		slog.Info("secret v1→v2 migration completed", "migrated_count", result.Migrated, "failed", len(result.Failed))
	}
	return result, nil
}
