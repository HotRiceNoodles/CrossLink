package service

import (
	"context"
	"errors"
	"testing"
	"time"

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
	require.NoError(t, db.AutoMigrate(&model.APIKey{}, &model.APIKeyHash{}, &model.Provider{}, &model.ProviderModel{}))
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
	// One registered global model so the allowed_models check can pass for it.
	require.NoError(t, db.Create(&model.Provider{Name: "test-p", Status: 1}).Error)
	var provider model.Provider
	require.NoError(t, db.Where("name = ?", "test-p").First(&provider).Error)
	require.NoError(t, db.Create(&model.ProviderModel{ProviderID: provider.ID, ModelName: "gpt-4o", ProviderModel: "gpt-4o", Status: 1}).Error)

	results := svc.Import(context.Background(), &ImportKeysInput{
		Items: []ImportKeyItem{
			{Key: "sk-good-vendor-key-000001", Vendor: "openai", Protocol: "openai", AllowedModels: []string{"gpt-4o"}},
			{Key: "sk-good-vendor-key-000001"},                                 // duplicate of item 0 (vendor/protocol optional now)
			{Key: "sk-good-vendor-key-000002", Vendor: "not-a-vendor"},         // invalid vendor (must be valid when provided)
			{Key: "sk-short", Vendor: "anthropic"},                             // invalid format
			{Key: "sk-good-vendor-key-000003", Vendor: "azure", AllowedModels: []string{"no-such-model"}}, // model missing
			{Key: "sk-good-vendor-key-000004", Vendor: "gemini"},               // no allowlist -> no model check
			{Key: "sk-good-vendor-key-000005", Protocol: "bogus"},              // invalid protocol
			{Key: "sk-good-vendor-key-000006"},                                 // no vendor, no protocol -> unrestricted
		},
	})

	require.Len(t, results, 8)
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

	require.Equal(t, ImportStatusInvalid, results[6].Status)

	// No vendor, no protocol: unrestricted key with an unprefixed default name.
	require.Equal(t, ImportStatusCreated, results[7].Status)
	require.Equal(t, "imported-sk-good", results[7].Name)

	// Protocol maps onto the route allowlist; absent protocol leaves it open.
	var restricted, open model.APIKey
	require.NoError(t, db.First(&restricted, results[0].KeyID).Error)
	require.JSONEq(t, `["openai"]`, string(restricted.AllowedRoutes))
	require.Equal(t, "openai", restricted.SourceVendor)
	require.NoError(t, db.First(&open, results[7].KeyID).Error)
	require.Empty(t, string(open.AllowedRoutes))
	require.Empty(t, open.SourceVendor)

	// Only the three valid items hit the DB.
	var count int64
	require.NoError(t, db.Model(&model.APIKey{}).Where("key_source = ?", model.KeySourceImported).Count(&count).Error)
	require.Equal(t, int64(3), count)
}

// TestImport_DuplicateDetectionCoversInactiveHashes locks in the fix for the
// live 23505 unique-constraint failure: re-importing a key whose hash row the
// active-key JOIN hides (disabled key, or a rotated-away legacy hash with
// expired grace) must report duplicate, not blow up on INSERT.
func TestImport_DuplicateDetectionCoversInactiveHashes(t *testing.T) {
	svc, db := setupKeyTestService(t)
	ctx := context.Background()

	// Scenario 1: previously imported, then disabled (status=2).
	created, err := svc.Create(ctx, &CreateKeyInput{Name: "disabled one", RawKey: "sk-disabled-vendor-key-01", Vendor: "openai"})
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.APIKey{}).Where("id = ?", created.ID).Update("status", 2).Error)

	results := svc.Import(ctx, &ImportKeysInput{Items: []ImportKeyItem{{Key: "sk-disabled-vendor-key-01"}}})
	require.Len(t, results, 1)
	require.Equal(t, ImportStatusDuplicate, results[0].Status)
	require.Equal(t, created.ID, results[0].ExistingKeyID)

	// Scenario 2: rotated away; the legacy hash row has expired grace.
	created2, err := svc.Create(ctx, &CreateKeyInput{Name: "rotated one", RawKey: "sk-rotated-vendor-key-002", Vendor: "openai"})
	require.NoError(t, err)
	_, err = svc.Regenerate(ctx, created2.ID) // old hash demoted with grace_until=now
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.APIKeyHash{}).
		Where("api_key_id = ? AND is_primary = false", created2.ID).
		Update("grace_until", time.Now().Add(-time.Minute)).Error)

	results = svc.Import(ctx, &ImportKeysInput{Items: []ImportKeyItem{{Key: "sk-rotated-vendor-key-002"}}})
	require.Len(t, results, 1)
	require.Equal(t, ImportStatusDuplicate, results[0].Status)
	require.Equal(t, created2.ID, results[0].ExistingKeyID)
}

// TestImport_AfterKeyDelete locks in the fix for the api_keys_key_hash_key
// 23505: deleting a key leaves a soft-deleted row that would hold the unique
// slot forever. Delete() must free the slot, and import must purge legacy
// tombstones that still hold it.
func TestImport_AfterKeyDelete(t *testing.T) {
	svc, db := setupKeyTestService(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, &CreateKeyInput{Name: "deleted import", RawKey: "sk-deleted-vendor-key-77", Vendor: "openai"})
	require.NoError(t, err)
	require.NoError(t, svc.Delete(ctx, created.ID))

	// Delete() freed the slot (tombstone hash on the soft-deleted row).
	var dead model.APIKey
	require.NoError(t, db.Unscoped().First(&dead, created.ID).Error)
	require.NotEqual(t, created.KeyHash, dead.KeyHash)

	// Re-importing the same raw key must succeed, not hit the unique index.
	results := svc.Import(ctx, &ImportKeysInput{Items: []ImportKeyItem{{Key: "sk-deleted-vendor-key-77", Vendor: "openai"}}})
	require.Len(t, results, 1)
	require.Equal(t, ImportStatusCreated, results[0].Status)

	// Legacy tombstone: simulate a row deleted before the fix (soft-deleted
	// row still holding the real hash) — import must purge and recreate.
	created2, err := svc.Create(ctx, &CreateKeyInput{Name: "legacy tombstone", RawKey: "sk-legacy-tombstone-key-9", Vendor: "openai"})
	require.NoError(t, err)
	require.NoError(t, db.Unscoped().Model(&model.APIKey{}).Where("id = ?", created2.ID).
		Updates(map[string]interface{}{"key_hash": created2.KeyHash, "deleted_at": time.Now()}).Error)
	require.NoError(t, db.Where("api_key_id = ?", created2.ID).Delete(&model.APIKeyHash{}).Error)

	results = svc.Import(ctx, &ImportKeysInput{Items: []ImportKeyItem{{Key: "sk-legacy-tombstone-key-9", Vendor: "openai"}}})
	require.Len(t, results, 1)
	require.Equal(t, ImportStatusCreated, results[0].Status, "legacy tombstone should be purged and key recreated: %+v", results[0])
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

// TestRotate_ImportedKeyGracesForbidden: even the Pro grace-rotate path
// (Rotate with gracePeriod > 0) must degenerate to zero grace for imported
// keys — the old hash string is the vendor credential and the operator
// revokes it at the vendor in the same action window.
func TestRotate_ImportedKeyGraceForcedToZero(t *testing.T) {
	svc, db := setupKeyTestService(t)
	ctx := context.Background()
	raw := "sk-grace-rotate-vendor-1"

	created, err := svc.Create(ctx, &CreateKeyInput{Name: "imported", RawKey: raw, Vendor: "openai"})
	require.NoError(t, err)

	// Contrast: a NATIVE key's requested grace is honored (old key still
	// validates within the grace window).
	native, err := svc.Create(ctx, &CreateKeyInput{Name: "native"})
	require.NoError(t, err)
	_, err = svc.Rotate(ctx, native.ID, time.Hour)
	require.NoError(t, err)
	_, err = svc.Validate(ctx, native.APIKey)
	require.NoError(t, err, "native key grace must be honored")

	// Imported key: grace request ignored, old key dead immediately.
	_, err = svc.Rotate(ctx, created.ID, 24*time.Hour)
	require.NoError(t, err)
	_, err = svc.Validate(ctx, raw)
	require.True(t, errors.Is(err, repository.ErrKeyNotFound), "grace rotation must be forced to zero for imported keys, got %v", err)

	var hashes []model.APIKeyHash
	require.NoError(t, db.Where("api_key_id = ?", created.ID).Find(&hashes).Error)
	for _, h := range hashes {
		if !h.IsPrimary {
			require.NotNil(t, h.GraceUntil)
			require.False(t, h.GraceUntil.After(time.Now().Add(time.Second)), "legacy hash grace must be now, got %v", h.GraceUntil)
		}
	}
}

// TestImport_ModelValidationIsOrgScoped: the model allowlist check must only
// see models reachable from the importing org (global or own providers).
func TestImport_ModelValidationIsOrgScoped(t *testing.T) {
	svc, db := setupKeyTestService(t)
	ctx := context.Background()

	org1 := int64(101)
	org2 := int64(202)
	require.NoError(t, db.Create(&model.Provider{Name: "global-p", Status: 1}).Error)
	require.NoError(t, db.Create(&model.Provider{Name: "org1-p", Status: 1, OrgID: &org1}).Error)
	require.NoError(t, db.Create(&model.Provider{Name: "org2-p", Status: 1, OrgID: &org2}).Error)
	for _, m := range []struct{ name, provider string }{
		{"shared-model", "global-p"}, {"org1-model", "org1-p"}, {"org2-model", "org2-p"},
	} {
		var p model.Provider
		require.NoError(t, db.Where("name = ?", m.provider).First(&p).Error)
		require.NoError(t, db.Create(&model.ProviderModel{ProviderID: p.ID, ModelName: m.name, ProviderModel: m.name, Status: 1}).Error)
	}

	// Org 1 importing: own + global models pass, org 2's model is missing.
	results := svc.Import(ctx, &ImportKeysInput{OrgID: org1, Items: []ImportKeyItem{
		{Key: "sk-org1-allowed-key-0001", AllowedModels: []string{"shared-model"}},
		{Key: "sk-org1-allowed-key-0002", AllowedModels: []string{"org1-model"}},
		{Key: "sk-org1-allowed-key-0003", AllowedModels: []string{"org2-model"}},
	}})
	require.Equal(t, ImportStatusCreated, results[0].Status)
	require.Equal(t, ImportStatusCreated, results[1].Status)
	require.Equal(t, ImportStatusModelMissing, results[2].Status)
	require.Equal(t, []string{"org2-model"}, results[2].MissingModels)
}

// TestImport_GatewayNativeKeyString: importing a string equal to an existing
// native gateway key must be reported as duplicate, not create a shadow.
func TestImport_GatewayNativeKeyString(t *testing.T) {
	svc, _ := setupKeyTestService(t)
	ctx := context.Background()

	native, err := svc.Create(ctx, &CreateKeyInput{Name: "native"})
	require.NoError(t, err)

	results := svc.Import(ctx, &ImportKeysInput{Items: []ImportKeyItem{{Key: native.APIKey}}})
	require.Len(t, results, 1)
	require.Equal(t, ImportStatusDuplicate, results[0].Status)
	require.Equal(t, native.ID, results[0].ExistingKeyID)
}
