package service

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

// ---- functional mock repo ----

type mockPatRepo struct {
	findByHash   func(ctx context.Context, hash string) (*model.PatToken, error)
	create       func(ctx context.Context, t *model.PatToken) error
	getByID      func(ctx context.Context, id int64) (*model.PatToken, error)
	revoke       func(ctx context.Context, id int64) error
	touchLastUsed func(ctx context.Context, id int64) error
}

func hashPat(plain string) string { return HashPatToken(plain) }

func (m *mockPatRepo) FindByHash(ctx context.Context, hash string) (*model.PatToken, error) {
	if m.findByHash != nil {
		return m.findByHash(ctx, hash)
	}
	return nil, gorm.ErrRecordNotFound
}
func (m *mockPatRepo) Create(ctx context.Context, t *model.PatToken) error {
	return m.create(ctx, t)
}
func (m *mockPatRepo) GetByID(ctx context.Context, id int64) (*model.PatToken, error) {
	return m.getByID(ctx, id)
}
func (m *mockPatRepo) Revoke(ctx context.Context, id int64) error {
	return m.revoke(ctx, id)
}

func (m *mockPatRepo) TouchLastUsed(ctx context.Context, id int64) error {
	if m.touchLastUsed != nil {
		return m.touchLastUsed(ctx, id)
	}
	return nil
}

func newTestPatService(repo *mockPatRepo) *PatService {
	return NewPatService(repo)
}

// ---- GenerateToken ----

func TestPATGenerateToken(t *testing.T) {
	s := newTestPatService(&mockPatRepo{})
	plain, hash, err := s.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(plain, "clpat_") {
		t.Errorf("plaintext should start with clpat_, got %s", plain)
	}
	if len(plain) <= len("clpat_") {
		t.Errorf("plaintext too short: %s", plain)
	}
	if len(hash) != 64 {
		t.Errorf("hash should be 64 hex chars, got %d", len(hash))
	}
	if _, err := hex.DecodeString(hash); err != nil {
		t.Errorf("hash not valid hex: %v", err)
	}
	plain2, _, _ := s.GenerateToken()
	if plain2 == plain {
		t.Errorf("two generated tokens should differ")
	}
}

// ---- Create ----

func TestPATCreate_ScopeExceeded(t *testing.T) {
	s := newTestPatService(&mockPatRepo{})
	_, err := s.Create(context.Background(), 1, []string{"budget:read"}, "n", []string{"health:read"})
	if !errors.Is(err, ErrScopeExceeded) {
		t.Errorf("want ErrScopeExceeded, got %v", err)
	}
}

func TestPATCreate_UnknownAction(t *testing.T) {
	s := newTestPatService(&mockPatRepo{})
	_, err := s.Create(context.Background(), 1, []string{"bogus:action"}, "n", []string{"bogus:action"})
	if !errors.Is(err, ErrScopeExceeded) {
		t.Errorf("unknown action should fail scope validation, got %v", err)
	}
}

func TestPATCreate_NoAllowedActions_FailClosed(t *testing.T) {
	s := newTestPatService(&mockPatRepo{create: func(_ context.Context, _ *model.PatToken) error {
		t.Error("repo Create must not be called")
		return nil
	}})
	if _, err := s.Create(context.Background(), 1, nil, "n", []string{"budget:read"}); !errors.Is(err, ErrScopeExceeded) {
		t.Errorf("nil allowedActions want ErrScopeExceeded, got %v", err)
	}
}

func TestPATCreate_EmptyScopes_Rejected(t *testing.T) {
	s := newTestPatService(&mockPatRepo{create: func(_ context.Context, _ *model.PatToken) error {
		t.Error("repo Create must not be called")
		return nil
	}})
	if _, err := s.Create(context.Background(), 1, []string{"budget:read"}, "n", nil); !errors.Is(err, ErrScopeExceeded) {
		t.Errorf("empty scopes want ErrScopeExceeded, got %v", err)
	}
}

func TestPATCreate_EmptyName(t *testing.T) {
	s := newTestPatService(&mockPatRepo{})
	_, err := s.Create(context.Background(), 1, []string{"budget:read"}, "", []string{"budget:read"})
	if !errors.Is(err, ErrInvalidName) {
		t.Errorf("want ErrInvalidName, got %v", err)
	}
}

func TestPATCreate_Success(t *testing.T) {
	var stored model.PatToken
	repo := &mockPatRepo{create: func(_ context.Context, tok *model.PatToken) error {
		stored = *tok
		return nil
	}}
	s := newTestPatService(repo)
	res, err := s.Create(context.Background(), 7, []string{"budget:read", "health:read"}, "ci", []string{"budget:read"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Plaintext == "" || strings.Contains(stored.TokenHash, res.Plaintext) {
		t.Errorf("hash stored must not contain plaintext")
	}
	if stored.TokenHash != hashPat(res.Plaintext) {
		t.Errorf("stored hash mismatch")
	}
	if stored.UserID != 7 {
		t.Errorf("user id = %d, want 7", stored.UserID)
	}
	d := time.Until(stored.ExpiresAt)
	if d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Errorf("expires_at not ~90d: %v", d)
	}
	if !strings.Contains(string(stored.Scopes), "budget:read") {
		t.Errorf("scopes not stored: %s", stored.Scopes)
	}
}

// ---- Validate ----

func TestPATValidate_BadPrefix(t *testing.T) {
	s := newTestPatService(&mockPatRepo{})
	if _, err := s.Validate(context.Background(), "sk-abc"); !errors.Is(err, ErrInvalidFormat) {
		t.Errorf("want ErrInvalidFormat, got %v", err)
	}
}

func TestPATValidate_NotFound(t *testing.T) {
	s := newTestPatService(&mockPatRepo{})
	if _, err := s.Validate(context.Background(), "clpat_whatever"); !errors.Is(err, ErrPatTokenNotFound) {
		t.Errorf("want ErrPatTokenNotFound, got %v", err)
	}
}

func TestPATValidate_StatusZero(t *testing.T) {
	tok := &model.PatToken{Status: 0, ExpiresAt: time.Now().Add(time.Hour)}
	s := newTestPatService(&mockPatRepo{findByHash: func(_ context.Context, _ string) (*model.PatToken, error) { return tok, nil }})
	if _, err := s.Validate(context.Background(), "clpat_x"); !errors.Is(err, ErrPatInactive) {
		t.Errorf("want ErrPatInactive, got %v", err)
	}
}

func TestPATValidate_RevokedAt(t *testing.T) {
	now := time.Now()
	tok := &model.PatToken{Status: 1, ExpiresAt: now.Add(time.Hour), RevokedAt: &now}
	s := newTestPatService(&mockPatRepo{findByHash: func(_ context.Context, _ string) (*model.PatToken, error) { return tok, nil }})
	if _, err := s.Validate(context.Background(), "clpat_x"); !errors.Is(err, ErrPatRevoked) {
		t.Errorf("want ErrPatRevoked, got %v", err)
	}
}

func TestPATValidate_Expired(t *testing.T) {
	tok := &model.PatToken{Status: 1, ExpiresAt: time.Now().Add(-time.Minute)}
	s := newTestPatService(&mockPatRepo{findByHash: func(_ context.Context, _ string) (*model.PatToken, error) { return tok, nil }})
	if _, err := s.Validate(context.Background(), "clpat_x"); !errors.Is(err, ErrPatExpired) {
		t.Errorf("want ErrPatExpired, got %v", err)
	}
}

func TestPATValidate_OK(t *testing.T) {
	plain := "clpat_goodtoken"
	tok := &model.PatToken{Status: 1, ExpiresAt: time.Now().Add(time.Hour), Scopes: []byte(`["budget:read"]`)}
	s := newTestPatService(&mockPatRepo{findByHash: func(_ context.Context, h string) (*model.PatToken, error) {
		if h != hashPat(plain) {
			return nil, gorm.ErrRecordNotFound
		}
		return tok, nil
	}})
	got, err := s.Validate(context.Background(), plain)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != tok {
		t.Errorf("token not returned")
	}
	scopes, err := got.ScopeList()
	if err != nil || len(scopes) != 1 || scopes[0] != "budget:read" {
		t.Errorf("ScopeList = %v, %v", scopes, err)
	}
}

// ---- Revoke ----

func TestPATRevoke_NotOwner(t *testing.T) {
	tok := &model.PatToken{ID: 5, UserID: 1, Status: 1}
	s := newTestPatService(&mockPatRepo{
		getByID: func(_ context.Context, _ int64) (*model.PatToken, error) { return tok, nil },
		revoke: func(_ context.Context, _ int64) error {
			t.Errorf("revoke must not be called for non-owner")
			return nil
		},
	})
	if err := s.Revoke(context.Background(), 5, 2); err == nil {
		t.Errorf("revoking another user's token should fail")
	}
}

func TestPATRevoke_Owner(t *testing.T) {
	tok := &model.PatToken{ID: 5, UserID: 1, Status: 1}
	revoked := false
	s := newTestPatService(&mockPatRepo{
		getByID: func(_ context.Context, _ int64) (*model.PatToken, error) { return tok, nil },
		revoke:  func(_ context.Context, _ int64) error { revoked = true; return nil },
	})
	if err := s.Revoke(context.Background(), 5, 1); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !revoked {
		t.Errorf("repo revoke not called")
	}
}
