package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

var ErrKeyExpired = errors.New("api key expired")

const authCachePrefix = "auth:key:"
const authCacheTTL = 60 * time.Second

type KeyService struct {
	repo     *repository.APIKeyRepo
	hashRepo *repository.APIKeyHashRepo
	db       *gorm.DB
	crypto   crypto.CryptoProvider
	rdb      redis.Cmdable
}

func NewKeyService(repo *repository.APIKeyRepo, hashRepo *repository.APIKeyHashRepo, db *gorm.DB, cp crypto.CryptoProvider, rdb redis.Cmdable) *KeyService {
	return &KeyService{repo: repo, hashRepo: hashRepo, db: db, crypto: cp, rdb: rdb}
}

type CreateKeyInput struct {
	Name            string
	Email           string
	AllowedModels   []string
	AllowedRoutes   []string
	TPMLimit        int
	RPMLimit        int
	MaxBudget       float64
	BudgetPeriod    string
	MaxCalls        int
	CallPeriod      string
	ExpiresAt       *time.Time
	PriceMultiplier *float64
	CreatedByID     int64
	TeamID          int64
	OrgID           int64
	RawKey          string // non-empty = register an existing vendor key as-is (equal-value key)
	Vendor          string // source vendor for imported keys (openai/anthropic/gemini/azure/other)
}

type CreateKeyResult struct {
	APIKey    string // plaintext, shown only once
	KeyPrefix string
	KeyHash   string
	ID        int64
}

type RotateResult struct {
	NewKey    string
	KeyPrefix string
}

func (s *KeyService) Create(ctx context.Context, input *CreateKeyInput) (*CreateKeyResult, error) {
	rawKey := input.RawKey
	if rawKey == "" {
		generated, err := generateRawKey()
		if err != nil {
			return nil, fmt.Errorf("generate key: %w", err)
		}
		rawKey = generated
	} else if err := ValidateImportedKey(rawKey); err != nil {
		return nil, fmt.Errorf("invalid imported key: %w", err)
	}
	keyHash := s.crypto.HashHex([]byte(rawKey))
	prefix := rawKey[:7]

	key := &model.APIKey{
		Name:            input.Name,
		KeyHash:         keyHash,
		KeyPrefix:       prefix,
		Status:          1,
		TPMLimit:        input.TPMLimit,
		RPMLimit:        input.RPMLimit,
		MaxBudget:       input.MaxBudget,
		BudgetPeriod:    input.BudgetPeriod,
		MaxCalls:        input.MaxCalls,
		CallPeriod:      input.CallPeriod,
		ExpiresAt:       input.ExpiresAt,
		PriceMultiplier: multOrDefault(input.PriceMultiplier),
	}
	if input.RawKey != "" {
		key.KeySource = model.KeySourceImported
		key.SourceVendor = input.Vendor
	}
	if len(input.AllowedModels) > 0 {
		key.AllowedModels, _ = json.Marshal(input.AllowedModels)
	}
	if len(input.AllowedRoutes) > 0 {
		key.AllowedRoutes, _ = json.Marshal(input.AllowedRoutes)
	}
	if input.CreatedByID > 0 {
		key.CreatedByID = &input.CreatedByID
	}
	if input.TeamID > 0 {
		key.TeamID = &input.TeamID
	}
	if input.OrgID > 0 {
		key.OrgID = &input.OrgID
	}
	if input.Email != "" {
		key.Email = &input.Email
	}

	hashRecord := &model.APIKeyHash{
		KeyHash:   keyHash,
		KeyPrefix: prefix,
		HashAlgo:  string(s.crypto.Algorithms().Hash),
		IsPrimary: true,
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(key).Error; err != nil {
			return fmt.Errorf("create api key: %w", err)
		}
		hashRecord.APIKeyID = key.ID
		if err := tx.Create(hashRecord).Error; err != nil {
			return fmt.Errorf("create hash record: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &CreateKeyResult{
		APIKey:    rawKey,
		KeyPrefix: prefix,
		KeyHash:   keyHash,
		ID:        key.ID,
	}, nil
}

func (s *KeyService) Validate(ctx context.Context, rawKey string) (*model.APIKey, error) {
	keyHash := s.crypto.HashHex([]byte(rawKey))

	// Try Redis cache first
	if s.rdb != nil {
		cached, err := s.rdb.Get(ctx, authCachePrefix+keyHash).Bytes()
		if err == nil {
			var key model.APIKey
			if json.Unmarshal(cached, &key) == nil {
				if key.ExpiresAt != nil && key.ExpiresAt.Before(time.Now()) {
					s.markExpiredDisabled(ctx, &key, keyHash)
					return nil, ErrKeyExpired
				}
				if key.Status != 1 {
					// Key was disabled after this entry was cached (the DB path
					// filters status = 1). Drop the stale entry and behave like
					// the DB lookup would have.
					s.invalidateCache(ctx, keyHash)
					return nil, repository.ErrKeyNotFound
				}
				return &key, nil
			}
		}
	}

	key, err := s.hashRepo.GetKeyByHash(ctx, keyHash)
	if err != nil {
		if !errors.Is(err, repository.ErrHashNotFound) {
			return nil, fmt.Errorf("lookup key by hash: %w", err)
		}
		// Fallback: try SHA-256 for legacy keys when in GM mode
		if s.crypto.Algorithms().Hash == crypto.AlgoSM3 {
			legacyHash := sha256.Sum256([]byte(rawKey))
			sha256Hex := hex.EncodeToString(legacyHash[:])
			key, err = s.hashRepo.GetKeyByHash(ctx, sha256Hex)
			if err != nil {
				if errors.Is(err, repository.ErrHashNotFound) {
					return nil, repository.ErrKeyNotFound
				}
				return nil, fmt.Errorf("lookup key by hash (legacy): %w", err)
			}
		} else {
			return nil, repository.ErrKeyNotFound
		}
	}

	if key.ExpiresAt != nil && key.ExpiresAt.Before(time.Now()) {
		s.markExpiredDisabled(ctx, key, keyHash)
		return nil, ErrKeyExpired
	}

	// Update last_used_at once per day (date-level granularity)
	now := time.Now()
	if key.LastUsedAt == nil || key.LastUsedAt.UTC().Format("2006-01-02") != now.UTC().Format("2006-01-02") {
		today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
		s.db.WithContext(ctx).Model(key).Update("last_used_at", today)
		key.LastUsedAt = &today
	}

	// Cache the result
	if s.rdb != nil {
		if data, err := json.Marshal(key); err == nil {
			s.rdb.Set(ctx, authCachePrefix+keyHash, data, authCacheTTL)
		}
	}

	return key, nil
}

// markExpiredDisabled 在确认密钥已过期时将其状态置为 disabled(2)。
// 用带 status=1 条件的直接 UPDATE，幂等且避免覆盖并发改动。
func (s *KeyService) markExpiredDisabled(ctx context.Context, key *model.APIKey, keyHash string) {
	if s.rdb != nil {
		s.rdb.Del(ctx, authCachePrefix+keyHash)
	}
	if err := s.db.WithContext(ctx).
		Model(&model.APIKey{}).
		Where("id = ? AND status = 1", key.ID).
		Update("status", 2).Error; err != nil {
		slog.Warn("auto-disable expired key failed", "id", key.ID, "error", err)
	}
}

func (s *KeyService) List(ctx context.Context, orgID int64) ([]model.APIKey, error) {
	return s.repo.List(ctx, orgID)
}

func (s *KeyService) ListByTeam(ctx context.Context, teamID int64) ([]model.APIKey, error) {
	return s.repo.ListByTeam(ctx, teamID)
}

func (s *KeyService) ListByCreator(ctx context.Context, userID int64) ([]model.APIKey, error) {
	return s.repo.ListByCreatorID(ctx, userID)
}

func (s *KeyService) GetByID(ctx context.Context, orgID int64, id int64) (*model.APIKey, error) {
	return s.repo.GetByID(ctx, orgID, id)
}

func (s *KeyService) Update(ctx context.Context, key *model.APIKey) error {
	if err := s.repo.Update(ctx, key); err != nil {
		return err
	}
	s.invalidateCache(ctx, key.KeyHash)
	return nil
}

func (s *KeyService) Delete(ctx context.Context, id int64) error {
	key, err := s.repo.GetByID(ctx, 0, id)
	if err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("api_key_id = ?", id).Delete(&model.APIKeyHash{}).Error; err != nil {
			return err
		}
		// Free the key_hash unique slot BEFORE soft-deleting: the tombstone row
		// would otherwise block re-importing the same raw key forever.
		if err := tx.Model(&model.APIKey{}).Where("id = ?", id).
			Update("key_hash", fmt.Sprintf("deleted-%d", id)).Error; err != nil {
			return err
		}
		return tx.Delete(&model.APIKey{}, id).Error
	}); err != nil {
		return err
	}
	s.invalidateCache(ctx, key.KeyHash)
	return nil
}

func (s *KeyService) Rotate(ctx context.Context, apiKeyID int64, gracePeriod time.Duration) (*RotateResult, error) {
	key, err := s.repo.GetByID(ctx, 0, apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("lookup key: %w", err)
	}

	rawKey, err := generateRawKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	keyHash := s.crypto.HashHex([]byte(rawKey))
	prefix := rawKey[:7]

	// Imported (equal-value) keys must never get a grace rotation: the old
	// hash string IS the vendor credential, and the operator revokes it at the
	// vendor platform in the same action window. Enforced here in the service
	// layer so every caller (zero-grace Regenerate, Pro grace-rotate API) is
	// covered, not just the wizard UI.
	if key.KeySource == model.KeySourceImported {
		gracePeriod = 0
	}
	graceUntil := time.Now().Add(gracePeriod)
	if gracePeriod == 0 {
		graceUntil = time.Now()
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.APIKeyHash{}).
			Where("api_key_id = ? AND is_primary = true", apiKeyID).
			Updates(map[string]interface{}{"is_primary": false, "grace_until": graceUntil})
		if result.Error != nil {
			return fmt.Errorf("set grace period on old hash: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return repository.ErrHashNotFound
		}

		newHash := &model.APIKeyHash{
			APIKeyID:  apiKeyID,
			KeyHash:   keyHash,
			KeyPrefix: prefix,
			IsPrimary: true,
			HashAlgo:  string(s.crypto.Algorithms().Hash),
		}
		if err := tx.Create(newHash).Error; err != nil {
			return fmt.Errorf("create new hash: %w", err)
		}

		updates := map[string]interface{}{
			"key_hash":   keyHash,
			"key_prefix": prefix,
		}
		if key.KeySource == model.KeySourceImported {
			// Imported (equal-value) keys must not linger in the transitional
			// state after rotation — flip back to native in the same transaction.
			updates["key_source"] = model.KeySourceNative
			updates["source_vendor"] = ""
		}
		if err := tx.Model(key).Updates(updates).Error; err != nil {
			return fmt.Errorf("update key: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	s.invalidateCache(ctx, key.KeyHash)
	s.invalidateCache(ctx, keyHash)

	return &RotateResult{NewKey: rawKey, KeyPrefix: prefix}, nil
}

func (s *KeyService) invalidateCache(ctx context.Context, keyHash string) {
	if s.rdb != nil && keyHash != "" {
		if err := s.rdb.Del(ctx, authCachePrefix+keyHash).Err(); err != nil {
			// A failed delete leaves a stale auth-cache entry live for up to
			// authCacheTTL — surface it instead of failing silently.
			slog.Warn("auth cache invalidation failed", "error", err)
		}
	}
}

func (s *KeyService) Regenerate(ctx context.Context, id int64) (*CreateKeyResult, error) {
	result, err := s.Rotate(ctx, id, 0)
	if err != nil {
		return nil, err
	}
	return &CreateKeyResult{
		APIKey:    result.NewKey,
		KeyPrefix: result.KeyPrefix,
	}, nil
}

func (s *KeyService) ListHashes(ctx context.Context, apiKeyID int64) ([]model.APIKeyHash, error) {
	return s.hashRepo.ListByAPIKeyID(ctx, apiKeyID)
}

// Import registers existing vendor keys as gateway keys as-is (equal-value
// keys, docs/plans/2026-10-11-zero-key-migration.md). Each item is processed
// independently; a failing item never blocks the rest.
type ImportKeyItem struct {
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	Vendor        string   `json:"vendor"`   // optional provenance only (rotation checklist: which platform to revoke at); no runtime meaning
	Protocol      string   `json:"protocol"` // optional route restriction: "openai" (/v1/chat/completions), "anthropic" (/v1/messages), "" = both
	TeamID        int64    `json:"team_id"`
	AllowedModels []string `json:"allowed_models"`
}

type ImportKeysInput struct {
	Items       []ImportKeyItem
	CreatedByID int64
	OrgID       int64
}

// Per-item import result statuses.
const (
	ImportStatusCreated      = "created"
	ImportStatusDuplicate    = "duplicate"
	ImportStatusInvalid      = "invalid"
	ImportStatusModelMissing = "model_not_found"
	ImportStatusFailed       = "failed"
)

type ImportKeyResult struct {
	Index          int      `json:"index"`
	Name           string   `json:"name,omitempty"`
	Status         string   `json:"status"`
	KeyPrefix      string   `json:"key_prefix,omitempty"`
	KeyID          int64    `json:"key_id,omitempty"`
	ExistingKeyID  int64    `json:"existing_key_id,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	MissingModels  []string `json:"missing_models,omitempty"`
}

var validImportVendors = map[string]bool{
	"openai": true, "anthropic": true, "gemini": true, "azure": true, "other": true,
}

// validImportProtocols maps the wizard's protocol choice to the key's
// AllowedRoutes values (same route types RequireRoute checks).
var validImportProtocols = map[string][]string{
	"openai":    {"openai"},
	"anthropic": {"anthropic"},
	"":          nil, // no restriction — both /v1/chat/completions and /v1/messages
}

// ValidateImportedKey enforces the floor validation for imported raw keys:
// 16-200 printable ASCII chars, no spaces. (Gateway cl- keys are 51 chars.)
func ValidateImportedKey(rawKey string) error {
	if len(rawKey) < 16 {
		return errors.New("key too short (min 16 chars)")
	}
	if len(rawKey) > 200 {
		return errors.New("key too long (max 200 chars)")
	}
	for i := 0; i < len(rawKey); i++ {
		if rawKey[i] < 0x21 || rawKey[i] > 0x7E {
			return errors.New("key contains non-printable or non-ASCII characters")
		}
	}
	return nil
}

// purgeSoftDeletedKeyByHash removes a soft-deleted api_keys row that still
// holds the key_hash unique slot (rows deleted before Delete() started
// freeing the slot). The row's hash records are already gone (Delete removes
// them) and every FK on api_keys is CASCADE or SET NULL, so a hard delete is
// safe and lets the same raw key be re-imported.
func (s *KeyService) purgeSoftDeletedKeyByHash(ctx context.Context, keyHash string) error {
	var dead model.APIKey
	err := s.db.WithContext(ctx).Unscoped().Where("key_hash = ?", keyHash).First(&dead).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if !dead.DeletedAt.Valid {
		return nil // live row — handled by the duplicate checks, not here
	}
	return s.db.WithContext(ctx).Unscoped().Delete(&model.APIKey{}, dead.ID).Error
}

func (s *KeyService) Import(ctx context.Context, input *ImportKeysInput) []ImportKeyResult {
	results := make([]ImportKeyResult, 0, len(input.Items))
	for i, item := range input.Items {
		res := ImportKeyResult{Index: i}
		if item.Vendor != "" && !validImportVendors[item.Vendor] {
			res.Status = ImportStatusInvalid
			res.Reason = "vendor must be one of openai, anthropic, gemini, azure, other"
			results = append(results, res)
			continue
		}
		routes, ok := validImportProtocols[item.Protocol]
		if !ok {
			res.Status = ImportStatusInvalid
			res.Reason = "protocol must be one of openai, anthropic, or empty"
			results = append(results, res)
			continue
		}
		if err := ValidateImportedKey(item.Key); err != nil {
			res.Status = ImportStatusInvalid
			res.Reason = err.Error()
			results = append(results, res)
			continue
		}
		// Duplicate pre-check: registering the same string twice is almost
		// certainly a re-run of the migration wizard, not a new key. The
		// key_hash unique index covers ALL hash rows, so after the
		// active-key lookup misses we must still check for rows the JOIN
		// hides (disabled keys, rotated-away legacy hashes) — otherwise the
		// INSERT below dies on the unique constraint.
		keyHash := s.crypto.HashHex([]byte(item.Key))
		if existing, err := s.hashRepo.GetKeyByHash(ctx, keyHash); err == nil {
			res.Status = ImportStatusDuplicate
			res.ExistingKeyID = existing.ID
			res.Reason = "key already registered on this gateway"
			results = append(results, res)
			continue
		} else if !errors.Is(err, repository.ErrHashNotFound) {
			res.Status = ImportStatusFailed
			res.Reason = "duplicate check failed"
			results = append(results, res)
			continue
		} else if any, err := s.hashRepo.FindAnyByHash(ctx, keyHash); err == nil {
			res.Status = ImportStatusDuplicate
			res.ExistingKeyID = any.APIKeyID
			res.Reason = "key hash already registered (its key is disabled or was rotated away)"
			results = append(results, res)
			continue
		} else if !errors.Is(err, repository.ErrHashNotFound) {
			res.Status = ImportStatusFailed
			res.Reason = "duplicate check failed"
			results = append(results, res)
			continue
		}
		// Legacy tombstones: keys deleted before Delete() freed the
		// api_keys.key_hash slot leave a soft-deleted row that still violates
		// the unique index on INSERT. Purge it so re-import works.
		if err := s.purgeSoftDeletedKeyByHash(ctx, keyHash); err != nil {
			res.Status = ImportStatusFailed
			res.Reason = "a deleted key still holds this hash and could not be purged"
			results = append(results, res)
			continue
		}
		name := item.Name
		if name == "" {
			if item.Vendor != "" {
				name = fmt.Sprintf("imported-%s-%s", item.Vendor, item.Key[:7])
			} else {
				name = fmt.Sprintf("imported-%s", item.Key[:7])
			}
		}
		res.Name = name
		// Optional model allowlist: verify every selected model exists in the
		// registry (org-scoped, mirroring FindByModelName) so the imported key
		// is not dead on arrival.
		if missing := s.missingModels(ctx, item.AllowedModels, input.OrgID); len(missing) > 0 {
			res.Status = ImportStatusModelMissing
			res.MissingModels = missing
			results = append(results, res)
			continue
		}
		created, err := s.Create(ctx, &CreateKeyInput{
			Name:          name,
			AllowedModels: item.AllowedModels,
			AllowedRoutes: routes,
			RawKey:        item.Key,
			Vendor:        item.Vendor,
			CreatedByID:   input.CreatedByID,
			TeamID:        item.TeamID,
			OrgID:         input.OrgID,
		})
		if err != nil {
			// Backstop for the concurrent-import race: if the row appeared
			// between the pre-check and this INSERT (unique constraint on either
			// api_key_hashes.key_hash or api_keys.key_hash), report duplicate
			// instead of a generic failure.
			if any, ferr := s.hashRepo.FindAnyByHash(ctx, keyHash); ferr == nil {
				res.Status = ImportStatusDuplicate
				res.ExistingKeyID = any.APIKeyID
				res.Reason = "key already registered (concurrent import)"
			} else {
				var holder model.APIKey
				if herr := s.db.WithContext(ctx).Unscoped().Where("key_hash = ?", keyHash).First(&holder).Error; herr == nil {
					res.Status = ImportStatusDuplicate
					res.ExistingKeyID = holder.ID
					res.Reason = "key already registered (concurrent import)"
				} else {
					res.Status = ImportStatusFailed
					res.Reason = "create key failed"
				}
			}
			results = append(results, res)
			continue
		}
		res.Status = ImportStatusCreated
		res.KeyPrefix = created.KeyPrefix
		res.KeyID = created.ID
		results = append(results, res)
	}
	return results
}

// missingModels returns the subset of names with no active provider_models row
// visible to the given org (global providers or the org's own; orgID 0 sees
// everything, mirroring FindByModelName). Best-effort: a lookup error does not
// block the import.
func (s *KeyService) missingModels(ctx context.Context, names []string, orgID int64) []string {
	if len(names) == 0 {
		return nil
	}
	var missing []string
	for _, name := range names {
		q := s.db.WithContext(ctx).Table("provider_models").
			Joins("JOIN providers ON providers.id = provider_models.provider_id AND providers.deleted_at IS NULL").
			Where("provider_models.model_name = ? AND provider_models.status = 1", name)
		if orgID != 0 {
			q = q.Where("providers.org_id IS NULL OR providers.org_id = ?", orgID)
		}
		var count int64
		if err := q.Count(&count).Error; err != nil {
			continue
		}
		if count == 0 {
			missing = append(missing, name)
		}
	}
	return missing
}

// GenerateRawKey produces a new random API key string (cl-<48 hex chars>).
// Exported so the onboarding handler can generate a key inside its own
// cross-table transaction without going through KeyService.Create.
func GenerateRawKey() (string, error) {
	return generateRawKey()
}

func generateRawKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand unavailable: %w", err)
	}
	return "cl-" + hex.EncodeToString(b), nil
}

// multOrDefault returns the multiplier value, defaulting to 1.0 if nil.
func multOrDefault(m *float64) float64 {
	if m == nil || *m <= 0 {
		return 1.0
	}
	return *m
}
