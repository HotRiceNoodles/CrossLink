package service

import (
	"context"
	"errors"
	"testing"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupKeyTestService(t *testing.T) (*KeyService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.APIKey{}, &model.APIKeyHash{}, &model.ProviderModel{}))
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)
	svc := NewKeyService(repository.NewAPIKeyRepo(db), repository.NewAPIKeyHashRepo(db), db, cp, nil)
	return svc, db
}

func TestCreate_WithRawKey_Imported(t *testing.T) {
	svc, db := setupKeyTestService(t)
	raw := "sk-existing-vendor-key-000111"

	result, err := svc.Create(context.Background(), &CreateKeyInput{
		Name:   "migrated app",
		RawKey: raw,
		Vendor: "openai",
	})
	require.NoError(t, err)
	require.Equal(t, raw, result.APIKey)
	require.Equal(t, raw[:7], result.KeyPrefix)
	require.NotZero(t, result.ID)

	var key model.APIKey
	require.NoError(t, db.First(&key, result.ID).Error)
	require.Equal(t, model.KeySourceImported, key.KeySource)
	require.Equal(t, "openai", key.SourceVendor)

	// The imported string authenticates like any gateway key.
	got, err := svc.Validate(context.Background(), raw)
	require.NoError(t, err)
	require.Equal(t, key.ID, got.ID)
}

func TestCreate_GeneratedKeyStaysNative(t *testing.T) {
	svc, db := setupKeyTestService(t)

	result, err := svc.Create(context.Background(), &CreateKeyInput{Name: "normal"})
	require.NoError(t, err)

	var key model.APIKey
	require.NoError(t, db.First(&key, result.ID).Error)
	require.Equal(t, model.KeySourceNative, key.KeySource)
	require.Empty(t, key.SourceVendor)
}

func TestCreate_WithRawKey_RejectsBadKeys(t *testing.T) {
	svc, _ := setupKeyTestService(t)
	cases := []struct {
		name string
		raw  string
	}{
		{"too short", "sk-short-123"},
		{"contains space", "sk-valid-length-key with space"},
		{"non-printable", "sk-valid-length-key-\x01abc"},
		{"non-ascii", "sk-valid-length-key-密钥"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(context.Background(), &CreateKeyInput{Name: "x", RawKey: tc.raw, Vendor: "openai"})
			require.Error(t, err)
		})
	}
}

func TestImport_PerItemResults(t *testing.T) {
	svc, db := setupKeyTestService(t)
	// One registered model so the allowed_models check can pass for it.
	require.NoError(t, db.Create(&model.ProviderModel{ModelName: "gpt-4o", ProviderModel: "gpt-4o", Status: 1}).Error)

	results := svc.Import(context.Background(), &ImportKeysInput{
		Items: []ImportKeyItem{
			{Key: "sk-good-vendor-key-000001", Vendor: "openai", AllowedModels: []string{"gpt-4o"}},
			{Key: "sk-good-vendor-key-000001", Vendor: "openai"},               // duplicate of item 0
			{Key: "sk-good-vendor-key-000002", Vendor: "not-a-vendor"},         // invalid vendor
			{Key: "sk-short", Vendor: "anthropic"},                             // invalid format
			{Key: "sk-good-vendor-key-000003", Vendor: "azure", AllowedModels: []string{"no-such-model"}}, // model missing
			{Key: "sk-good-vendor-key-000004", Vendor: "gemini"},               // no allowlist -> no model check
		},
	})

	require.Len(t, results, 6)
	require.Equal(t, ImportStatusCreated, results[0].Status)
	require.Equal(t, "sk-good", results[0].KeyPrefix)

	require.Equal(t, ImportStatusDuplicate, results[1].Status)
	require.Equal(t, results[0].KeyID, results[1].ExistingKeyID)

	require.Equal(t, ImportStatusInvalid, results[2].Status)
	require.Equal(t, ImportStatusInvalid, results[3].Status)

	require.Equal(t, ImportStatusModelMissing, results[4].Status)
	require.Equal(t, []string{"no-such-model"}, results[4].MissingModels)

	require.Equal(t, ImportStatusCreated, results[5].Status)
	require.Equal(t, "imported-gemini-sk-good", results[5].Name)

	// Only the two valid items hit the DB.
	var count int64
	require.NoError(t, db.Model(&model.APIKey{}).Where("key_source = ?", model.KeySourceImported).Count(&count).Error)
	require.Equal(t, int64(2), count)
}

func TestRegenerate_ImportedKey_ImmediateInvalidationAndMarkerCleared(t *testing.T) {
	svc, db := setupKeyTestService(t)
	raw := "sk-to-rotate-vendor-key-9"

	created, err := svc.Create(context.Background(), &CreateKeyInput{Name: "old", RawKey: raw, Vendor: "openai"})
	require.NoError(t, err)

	rotated, err := svc.Regenerate(context.Background(), created.ID)
	require.NoError(t, err)
	require.NotEqual(t, raw, rotated.APIKey)
	require.Equal(t, "cl-", rotated.APIKey[:3])

	// Old equal-value key must be dead immediately (zero grace).
	_, err = svc.Validate(context.Background(), raw)
	require.True(t, errors.Is(err, repository.ErrKeyNotFound), "expected ErrKeyNotFound, got %v", err)

	// New key authenticates; marker flipped back to native in the same transaction.
	got, err := svc.Validate(context.Background(), rotated.APIKey)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)

	var key model.APIKey
	require.NoError(t, db.First(&key, created.ID).Error)
	require.Equal(t, model.KeySourceNative, key.KeySource)
	require.Empty(t, key.SourceVendor)
}
