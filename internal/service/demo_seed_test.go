package service

import (
	"testing"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const demoKeyLiteral = "cl-demo-0000-0000-0000-0000"

func setupDemoSeedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Provider{}, &model.ProviderModel{}, &model.APIKey{}, &model.APIKeyHash{}))
	return db
}

func TestEnsureDemoSeed_CreatesProviderModelsKey(t *testing.T) {
	db := setupDemoSeedDB(t)
	cp, _ := crypto.NewProvider("standard")
	require.NoError(t, EnsureDemoSeed(db, "sqlite", demoKeyLiteral, cp))

	var p model.Provider
	require.NoError(t, db.Where("name = ?", "mock-demo").First(&p).Error)
	assert.Equal(t, "mock", p.AdapterType)
	assert.Equal(t, int16(1), p.Status)

	var models []model.ProviderModel
	db.Find(&models)
	assert.Equal(t, 2, len(models), "two mock models")

	var key model.APIKey
	require.NoError(t, db.Where("key_prefix = ?", "cl-demo").First(&key).Error)
	assert.Equal(t, cp.HashHex([]byte(demoKeyLiteral)), key.KeyHash)
	// allowed_models + allowed_routes populated
	assert.Contains(t, string(key.AllowedModels), "mock-sonnet")
	assert.Contains(t, string(key.AllowedRoutes), "anthropic")
	assert.Contains(t, string(key.AllowedRoutes), "openai")

	var hash model.APIKeyHash
	require.NoError(t, db.Where("api_key_id = ?", key.ID).First(&hash).Error)
	assert.Equal(t, key.KeyHash, hash.KeyHash)
	assert.True(t, hash.IsPrimary)
}

func TestEnsureDemoSeed_Idempotent(t *testing.T) {
	db := setupDemoSeedDB(t)
	cp, _ := crypto.NewProvider("standard")
	require.NoError(t, EnsureDemoSeed(db, "sqlite", demoKeyLiteral, cp))
	require.NoError(t, EnsureDemoSeed(db, "sqlite", demoKeyLiteral, cp)) // second call

	var provCount, modelCount, keyCount, hashCount int64
	db.Model(&model.Provider{}).Count(&provCount)
	db.Model(&model.ProviderModel{}).Count(&modelCount)
	db.Model(&model.APIKey{}).Where("key_prefix = ?", "cl-demo").Count(&keyCount)
	db.Model(&model.APIKeyHash{}).Where("key_prefix = ?", "cl-demo").Count(&hashCount)
	assert.Equal(t, int64(1), provCount, "provider not duplicated")
	assert.Equal(t, int64(2), modelCount, "models not duplicated")
	assert.Equal(t, int64(1), keyCount, "key not duplicated")
	assert.Equal(t, int64(1), hashCount, "hash not duplicated")
}

func TestEnsureDemoSeed_RefusesNonSQLite(t *testing.T) {
	db := setupDemoSeedDB(t)
	cp, _ := crypto.NewProvider("standard")
	err := EnsureDemoSeed(db, "postgres", demoKeyLiteral, cp)
	require.Error(t, err)
	var keyCount int64
	db.Model(&model.APIKey{}).Count(&keyCount)
	assert.Equal(t, int64(0), keyCount, "no key seeded on non-sqlite")
}

func TestEnsureDemoSeed_RefusesExistingNonMockProvider(t *testing.T) {
	db := setupDemoSeedDB(t)
	cp, _ := crypto.NewProvider("standard")
	// Pre-existing real provider
	db.Create(&model.Provider{Name: "openai-prod", DisplayName: "OpenAI", AdapterType: "openai", BaseURL: "https://api.openai.com", APIKey: "x", Status: 1})

	err := EnsureDemoSeed(db, "sqlite", demoKeyLiteral, cp)
	require.Error(t, err)
	var keyCount int64
	db.Model(&model.APIKey{}).Where("key_prefix = ?", "cl-demo").Count(&keyCount)
	assert.Equal(t, int64(0), keyCount, "no demo key seeded when real provider exists")
}

func TestEnsureDemoSeed_DemoKeyAllowListBounded(t *testing.T) {
	db := setupDemoSeedDB(t)
	cp, _ := crypto.NewProvider("standard")
	require.NoError(t, EnsureDemoSeed(db, "sqlite", demoKeyLiteral, cp))

	var key model.APIKey
	require.NoError(t, db.Where("key_prefix = ?", "cl-demo").First(&key).Error)
	assert.Contains(t, string(key.AllowedModels), "mock-sonnet")
	assert.Contains(t, string(key.AllowedModels), "mock-gpt4")
	assert.NotContains(t, string(key.AllowedModels), "gpt-4", "demo key must not allow real models")
}
