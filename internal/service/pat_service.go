package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"gorm.io/gorm"
)

var (
	ErrPatTokenNotFound = errors.New("pat token not found")
	ErrPatRevoked       = errors.New("pat token revoked")
	ErrPatInactive      = errors.New("pat token disabled")
	ErrPatExpired       = errors.New("pat token expired")
	ErrInvalidFormat    = errors.New("invalid pat token format")
	ErrScopeExceeded    = errors.New("scope exceeds caller permissions")
	ErrInvalidName      = errors.New("pat token name is required")
)

const (
	PatTokenPrefix = "clpat_"
	patExpiry    = 90 * 24 * time.Hour
	PatHashAlgo  = "sha256"
)

// PatTokenRepo is the repository dependency of PatService.
type PatTokenRepo interface {
	FindByHash(ctx context.Context, hash string) (*model.PatToken, error)
	Create(ctx context.Context, t *model.PatToken) error
	GetByID(ctx context.Context, id int64) (*model.PatToken, error)
	Revoke(ctx context.Context, id int64) error
	TouchLastUsed(ctx context.Context, id int64) error
}

type PatService struct {
	repo PatTokenRepo
}

func NewPatService(repo PatTokenRepo) *PatService {
	return &PatService{repo: repo}
}

type CreatePatResult struct {
	Token     *model.PatToken
	Plaintext string
}

// HashPatToken computes the unsalted SHA-256 hex hash of a PAT plaintext.
func HashPatToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// GenerateToken returns a new random plaintext token and its hash.
func (s *PatService) GenerateToken() (plaintext, hash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	plaintext = PatTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return plaintext, HashPatToken(plaintext), nil
}

// Create validates scopes against the caller's effective actions and the
// canonical ValidActions set, then stores a new PAT with a 90-day expiry.
func (s *PatService) Create(ctx context.Context, userID int64, allowedActions []string, name string, scopes []string) (*CreatePatResult, error) {
	if strings.TrimSpace(name) == "" {
		return nil, ErrInvalidName
	}
	// Fail-closed: without known caller permissions no scope can be validated,
	// and a PAT with no scopes is meaningless — reject both.
	if len(allowedActions) == 0 || len(scopes) == 0 {
		return nil, ErrScopeExceeded
	}
	allowed := make(map[string]bool, len(allowedActions))
	for _, a := range allowedActions {
		allowed[a] = true
	}
	for _, sc := range scopes {
		if !model.IsValidAction(sc) || !allowed[sc] {
			return nil, ErrScopeExceeded
		}
	}
	plaintext, hash, err := s.GenerateToken()
	if err != nil {
		return nil, err
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tok := &model.PatToken{
		UserID:    userID,
		Name:      name,
		TokenHash: hash,
		Scopes:    scopesJSON,
		Status:    1,
		ExpiresAt: now.Add(patExpiry),
		CreatedAt: now,
	}
	if err := s.repo.Create(ctx, tok); err != nil {
		return nil, err
	}
	return &CreatePatResult{Token: tok, Plaintext: plaintext}, nil
}

// Validate checks a plaintext PAT and returns the token record if active.
func (s *PatService) Validate(ctx context.Context, plaintext string) (*model.PatToken, error) {
	if !strings.HasPrefix(plaintext, PatTokenPrefix) || len(plaintext) <= len(PatTokenPrefix) {
		return nil, ErrInvalidFormat
	}
	tok, err := s.repo.FindByHash(ctx, HashPatToken(plaintext))
	if err != nil {
		if errors.Is(err, repository.ErrPatTokenNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrPatTokenNotFound
		}
		return nil, err
	}
	if tok.Status != 1 {
		return nil, ErrPatInactive
	}
	if tok.RevokedAt != nil {
		return nil, ErrPatRevoked
	}
	if time.Now().After(tok.ExpiresAt) {
		return nil, ErrPatExpired
	}
	return tok, nil
}

// TouchLastUsed delegates to the repo's throttled last_used_at update.
func (s *PatService) TouchLastUsed(ctx context.Context, id int64) error {
	return s.repo.TouchLastUsed(ctx, id)
}

// Revoke revokes a PAT owned by userID. Ownership is enforced; admin
// overrides are the handler's concern.
func (s *PatService) Revoke(ctx context.Context, id, userID int64) error {
	tok, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrPatTokenNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrPatTokenNotFound
		}
		return err
	}
	if tok.UserID != userID {
		return ErrPatTokenNotFound
	}
	return s.repo.Revoke(ctx, id)
}
