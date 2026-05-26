package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	Name          string
	Email         string
	AllowedModels []string
	AllowedRoutes []string
	TPMLimit      int
	RPMLimit      int
	MaxBudget     float64
	BudgetPeriod  string
	ExpiresAt     *time.Time
	CreatedByID   int64
	TeamID        int64
}

type CreateKeyResult struct {
	APIKey    string // plaintext, shown only once
	KeyPrefix string
	KeyHash   string
}

type RotateResult struct {
	NewKey    string
	KeyPrefix string
}

func (s *KeyService) Create(ctx context.Context, input *CreateKeyInput) (*CreateKeyResult, error) {
	rawKey, err := generateRawKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	keyHash := s.crypto.HashHex([]byte(rawKey))
	prefix := rawKey[:7]

	key := &model.APIKey{
		Name:          input.Name,
		KeyHash:       keyHash,
		KeyPrefix:     prefix,
		Status:        1,
		TPMLimit:      input.TPMLimit,
		RPMLimit:      input.RPMLimit,
		MaxBudget:     input.MaxBudget,
		BudgetPeriod:  input.BudgetPeriod,
		ExpiresAt:     input.ExpiresAt,
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
	if input.Email != "" {
		key.Email = &input.Email
	}

	hashRecord := &model.APIKeyHash{
		KeyHash:   keyHash,
		KeyPrefix: prefix,
		HashAlgo:  string(s.crypto.Algorithms().Hash),
		IsPrimary: true,
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
					s.rdb.Del(ctx, authCachePrefix+keyHash)
					return nil, ErrKeyExpired
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

func (s *KeyService) List(ctx context.Context) ([]model.APIKey, error) {
	return s.repo.List(ctx)
}

func (s *KeyService) ListByTeam(ctx context.Context, teamID int64) ([]model.APIKey, error) {
	return s.repo.ListByTeam(ctx, teamID)
}

func (s *KeyService) ListByCreator(ctx context.Context, userID int64) ([]model.APIKey, error) {
	return s.repo.ListByCreatorID(ctx, userID)
}

func (s *KeyService) GetByID(ctx context.Context, id int64) (*model.APIKey, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *KeyService) Update(ctx context.Context, key *model.APIKey) error {
	if err := s.repo.Update(ctx, key); err != nil {
		return err
	}
	s.invalidateCache(ctx, key.KeyHash)
	return nil
}

func (s *KeyService) Delete(ctx context.Context, id int64) error {
	key, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("api_key_id = ?", id).Delete(&model.APIKeyHash{}).Error; err != nil {
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
	key, err := s.repo.GetByID(ctx, apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("lookup key: %w", err)
	}

	rawKey, err := generateRawKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	keyHash := s.crypto.HashHex([]byte(rawKey))
	prefix := rawKey[:7]

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

		if err := tx.Model(key).Updates(map[string]interface{}{
			"key_hash":   keyHash,
			"key_prefix": prefix,
		}).Error; err != nil {
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
		s.rdb.Del(ctx, authCachePrefix+keyHash)
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

func generateRawKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand unavailable: %w", err)
	}
	return "cl-" + hex.EncodeToString(b), nil
}
