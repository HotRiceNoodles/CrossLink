package configio

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupConfigioDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Provider{}, &model.ProviderModel{}, &model.ErrorClassificationRule{}))
	return db
}

func encStoreForTest(t *testing.T) *secret.EncryptedDBStore {
	t.Helper()
	cp, _ := crypto.NewProvider("standard")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x07}, 32))
	store, err := secret.NewEncryptedDBStore(key, cp)
	require.NoError(t, err)
	return store
}

func TestBuildAndApplyRoundTrip(t *testing.T) {
	src := setupConfigioDB(t)
	store := encStoreForTest(t)

	// Seed source: 1 provider (encrypted api_key) + 2 models + 1 error rule.
	encKey, err := store.Encrypt("sk-source-secret")
	require.NoError(t, err)
	require.NoError(t, src.Create(&model.Provider{
		Name: "openai", DisplayName: "OpenAI", AdapterType: "openai_compatible",
		BaseURL: "https://api.openai.com/v1", APIKey: encKey, Status: 1,
	}).Error)
	var pid int64
	src.Model(&model.Provider{}).Where("name = ?", "openai").Pluck("id", &pid)
	require.NoError(t, src.Create(&model.ProviderModel{
		ProviderID: pid, ModelName: "gpt-4o", ProviderModel: "gpt-4o",
		Weight: 1, Priority: 1, Currency: "USD", RoutingStrategy: "weighted_random", Status: 1,
	}).Error)
	require.NoError(t, src.Create(&model.ProviderModel{
		ProviderID: pid, ModelName: "gpt-4o-mini", ProviderModel: "gpt-4o-mini",
		Currency: "USD", RoutingStrategy: "weighted_random", Status: 1,
	}).Error)
	require.NoError(t, src.Create(&model.ErrorClassificationRule{
		MatchField: "status", Pattern: "429", Classification: "quota",
		Scope: "account", Priority: 100, Enabled: true,
	}).Error)

	// Build bundle from source.
	bundle, warns, err := BuildBundle(src, store)
	require.NoError(t, err)
	require.Empty(t, warns)
	require.Len(t, bundle.Providers, 1)
	require.Len(t, bundle.Models, 2)
	require.Len(t, bundle.ErrorRules, 1)
	assert.Equal(t, "sk-source-secret", bundle.Providers[0].APIKey, "api_key must be decrypted to plaintext in bundle")
	assert.Equal(t, "openai", bundle.Models[0].ProviderName, "model must reference provider by name")

	// Apply to a fresh target DB (different encStore = different CL_ENCRYPTION_KEY).
	dst := setupConfigioDB(t)
	targetStore := encStoreForTest(t) // different key bytes would be ideal but same lib is fine for this test
	report, err := ApplyBundle(dst, targetStore, bundle, false)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Created.Providers)
	assert.Equal(t, 2, report.Created.Models)
	assert.Equal(t, 1, report.Created.ErrorRules)
	assert.Empty(t, report.Errors)
	assert.Empty(t, report.Skipped)

	// Target provider's api_key must be re-encrypted (not plaintext, not the source ciphertext).
	var tgtProvider model.Provider
	require.NoError(t, dst.Where("name = ?", "openai").First(&tgtProvider).Error)
	assert.NotEqual(t, "sk-source-secret", tgtProvider.APIKey, "must not store plaintext")
	dec, err := targetStore.Decrypt(tgtProvider.APIKey)
	require.NoError(t, err)
	assert.Equal(t, "sk-source-secret", dec, "target must decrypt back to original")

	// Re-apply → all skipped (idempotent at provider level).
	report2, err := ApplyBundle(dst, targetStore, bundle, false)
	require.NoError(t, err)
	assert.Equal(t, 0, report2.Created.Providers)
	assert.Equal(t, 0, report2.Created.Models)
	assert.Equal(t, 0, report2.Created.ErrorRules)
	assert.Len(t, report2.Skipped, 4) // 1 provider + 2 models + 1 rule
}

func TestBuildBundle_PlaintextDeployment(t *testing.T) {
	db := setupConfigioDB(t)
	// No encStore (nil) — provider stores plaintext api_key.
	require.NoError(t, db.Create(&model.Provider{
		Name: "deepseek", DisplayName: "DeepSeek", AdapterType: "openai_compatible",
		BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-plain", Status: 1,
	}).Error)

	bundle, warns, err := BuildBundle(db, nil)
	require.NoError(t, err)
	require.Empty(t, warns)
	require.Len(t, bundle.Providers, 1)
	assert.Equal(t, "sk-plain", bundle.Providers[0].APIKey)
}

func TestBuildBundle_EncProviderButNoEncStore(t *testing.T) {
	db := setupConfigioDB(t)
	store := encStoreForTest(t)
	enc, _ := store.Encrypt("sk-secret")
	require.NoError(t, db.Create(&model.Provider{
		Name: "p", DisplayName: "P", AdapterType: "openai_compatible",
		BaseURL: "https://x", APIKey: enc, Status: 1,
	}).Error)

	// Build with nil encStore → enc:// unresolvable → provider skipped with warning.
	bundle, warns, err := BuildBundle(db, nil)
	require.NoError(t, err)
	assert.Empty(t, bundle.Providers)
	require.Len(t, warns, 1)
}

func TestEnvReferenceSurvivesRoundTrip(t *testing.T) {
	// env:// reference must be preserved verbatim across export+import (not resolved).
	src := setupConfigioDB(t)
	require.NoError(t, src.Create(&model.Provider{
		Name: "p", DisplayName: "P", AdapterType: "openai_compatible",
		BaseURL: "https://x", APIKey: "env://MY_UPSTREAM_KEY", Status: 1,
	}).Error)

	bundle, _, err := BuildBundle(src, encStoreForTest(t))
	require.NoError(t, err)
	require.Equal(t, "env://MY_UPSTREAM_KEY", bundle.Providers[0].APIKey)

	dst := setupConfigioDB(t)
	report, err := ApplyBundle(dst, encStoreForTest(t), bundle, false)
	require.NoError(t, err)
	require.Empty(t, report.Errors)

	var p model.Provider
	require.NoError(t, dst.Where("name = ?", "p").First(&p).Error)
	assert.Equal(t, "env://MY_UPSTREAM_KEY", p.APIKey, "env:// must be stored verbatim, not resolved")
}

func TestApplyDryRun(t *testing.T) {
	src := setupConfigioDB(t)
	require.NoError(t, src.Create(&model.Provider{
		Name: "p", DisplayName: "P", AdapterType: "openai_compatible",
		BaseURL: "https://x", APIKey: "sk-plain", Status: 1,
	}).Error)
	bundle, _, err := BuildBundle(src, nil)
	require.NoError(t, err)

	dst := setupConfigioDB(t)
	report, err := ApplyBundle(dst, nil, bundle, true)
	require.NoError(t, err)
	assert.True(t, report.DryRun)
	assert.Equal(t, 1, report.Created.Providers)

	// Dry-run must not actually persist.
	var cnt int64
	dst.Model(&model.Provider{}).Count(&cnt)
	assert.EqualValues(t, 0, cnt, "dry-run must not write to DB")
}
