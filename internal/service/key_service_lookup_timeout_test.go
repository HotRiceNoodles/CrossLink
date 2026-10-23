package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
)

// hangingKeyHashRepo blocks every method until its ctx is done — the
// accept-hold blackhole shape measured in r78e (dial completes, protocol
// read never answers).
type hangingKeyHashRepo struct{}

func (hangingKeyHashRepo) GetKeyByHash(ctx context.Context, hash string) (*model.APIKey, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (hangingKeyHashRepo) FindAnyByHash(ctx context.Context, hash string) (*model.APIKeyHash, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (hangingKeyHashRepo) ListByAPIKeyID(ctx context.Context, apiKeyID int64) ([]model.APIKeyHash, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A cold key against a hung DB backend must surface ErrAuthUnavailable (the
// middleware's typed 503 class — fail-closed) within the configured bound,
// not wait for the client to hang up.
func TestValidate_AuthLookupTimeout_BoundedUnavailable(t *testing.T) {
	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatalf("crypto: %v", err)
	}
	svc := &KeyService{hashRepo: hangingKeyHashRepo{}, crypto: cp} // rdb nil -> DB path
	svc.SetAuthLookupTimeout(120 * time.Millisecond)

	start := time.Now()
	_, err = svc.Validate(context.Background(), "sk-hang-test")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from bounded hang")
	}
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("expected ErrAuthUnavailable (typed 503 class), got %v", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("bound not applied: Validate took %v", elapsed)
	}
}

func TestValidate_AuthLookupTimeout_Default2s(t *testing.T) {
	svc := &KeyService{}
	lctx, cancel := svc.lookupCtx(context.Background())
	defer cancel()
	dl, ok := lctx.Deadline()
	if !ok {
		t.Fatal("lookupCtx returned no deadline")
	}
	if d := time.Until(dl); d <= 0 || d > 2*time.Second {
		t.Fatalf("default bound not ~2s: %v", d)
	}
}

type errKeyHashRepo struct{ err error }

func (r *errKeyHashRepo) GetKeyByHash(ctx context.Context, hash string) (*model.APIKey, error) {
	return nil, r.err
}
func (r *errKeyHashRepo) FindAnyByHash(ctx context.Context, hash string) (*model.APIKeyHash, error) {
	return nil, r.err
}
func (r *errKeyHashRepo) ListByAPIKeyID(ctx context.Context, apiKeyID int64) ([]model.APIKeyHash, error) {
	return nil, r.err
}

// mapKeyHashRepo answers by hash — the SM3-miss/SHA256-hit shape of a GM
// deployment holding pre-migration legacy keys.
type mapKeyHashRepo struct{ byHash map[string]*model.APIKey }

func (m mapKeyHashRepo) GetKeyByHash(ctx context.Context, hash string) (*model.APIKey, error) {
	if k, ok := m.byHash[hash]; ok {
		return k, nil
	}
	return nil, repository.ErrHashNotFound
}
func (m mapKeyHashRepo) FindAnyByHash(ctx context.Context, hash string) (*model.APIKeyHash, error) {
	return nil, repository.ErrHashNotFound
}
func (m mapKeyHashRepo) ListByAPIKeyID(ctx context.Context, apiKeyID int64) ([]model.APIKeyHash, error) {
	return nil, nil
}

// Regression (source review 2026-10-22): sampling lctx.Err() AFTER cancel()
// planted Canceled on every in-bound call, misclassifying a genuine
// ErrHashNotFound as ErrAuthUnavailable (503) and making the SM3->SHA256
// fallback unreachable. This is the exact shape that must stay a 401-class
// miss under the standard provider.
func TestValidate_RealMiss_StaysErrKeyNotFound(t *testing.T) {
	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatalf("crypto: %v", err)
	}
	svc := &KeyService{hashRepo: &errKeyHashRepo{err: repository.ErrHashNotFound}, crypto: cp}
	svc.SetAuthLookupTimeout(500 * time.Millisecond)
	_, err = svc.Validate(context.Background(), "sk-real-miss")
	if !errors.Is(err, repository.ErrKeyNotFound) {
		t.Fatalf("real in-bound miss must be ErrKeyNotFound (401 class), got %v", err)
	}
	if errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("real miss misclassified as backend-unavailable (503 class)")
	}
}

// SM3 mode with a legacy SHA256 key: primary SM3 hash misses, the legacy
// lookup hits — the key MUST be returned (fallback reachable, bound wraps
// but does not break it).
func TestValidate_SM3LegacyFallback_Success(t *testing.T) {
	cp, err := crypto.NewProvider("gm")
	if err != nil {
		t.Fatalf("crypto gm: %v", err)
	}
	rawKey := "sk-gm-legacy-key"
	legacySum := sha256.Sum256([]byte(rawKey))
	legacyHex := hex.EncodeToString(legacySum[:])
	today := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), time.Now().UTC().Day(), 0, 0, 0, 0, time.UTC)
	legacyKey := &model.APIKey{ID: 4242, Status: 1, LastUsedAt: &today}
	repo := &mapKeyHashRepo{byHash: map[string]*model.APIKey{legacyHex: legacyKey}}

	svc := &KeyService{hashRepo: repo, crypto: cp}
	svc.SetAuthLookupTimeout(500 * time.Millisecond)
	key, err := svc.Validate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("legacy fallback must succeed, got %v", err)
	}
	if key == nil || key.ID != 4242 {
		t.Fatalf("expected legacy key 4242, got %+v", key)
	}
}

// The 503 class is reserved for the BOUND actually firing (blackhole), not
// for misses — covered above; here the bound fire itself is pinned.
func TestValidate_Hang_IsErrAuthUnavailable(t *testing.T) {
	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatalf("crypto: %v", err)
	}
	svc := &KeyService{hashRepo: hangingKeyHashRepo{}, crypto: cp}
	svc.SetAuthLookupTimeout(120 * time.Millisecond)
	_, err = svc.Validate(context.Background(), "sk-hang-503")
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("hung backend must be ErrAuthUnavailable (503 class), got %v", err)
	}
	if errors.Is(err, repository.ErrKeyNotFound) {
		t.Fatalf("hung backend must not be a credential verdict")
	}
}
