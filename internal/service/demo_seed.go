package service

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

// Demo provider/key constants. The demo key is a FIXED literal (embedded in the
// /demo try page JS), NOT generated — so do not call GenerateRawKey here.
const (
	demoProviderName = "mock-demo"
	demoKeyPrefix    = "cl-demo" // first 7 chars of demoKeyLiteral
)

var demoModels = []string{"mock-sonnet", "mock-gpt4"}

// EnsureDemoSeed idempotently seeds a mock provider + 2 mock models + a demo
// API key for the zero-cost demo mode. It creates NO admin user and NO usage
// logs. The demo key is the fixed literal (needed to embed in the /demo page).
//
// Safety guards (refuse to seed rather than risk injecting into a real deployment):
//   - driver must be sqlite (demo is single-container, no external deps)
//   - refuse if a non-mock provider already exists
//
// Idempotent: keyed on provider name + key prefix, so re-runs are a no-op.
func EnsureDemoSeed(db *gorm.DB, driver, demoAPIKey string, cp crypto.CryptoProvider) error {
	if driver != "sqlite" {
		return errors.New("demo mode requires sqlite database driver")
	}
	if demoAPIKey == "" {
		return errors.New("demo api key is empty")
	}

	// Refuse if a real (non-mock) provider already exists — avoids injecting a
	// public demo key into a gateway that already routes to billed backends.
	var realProviders int64
	if err := db.Model(&model.Provider{}).Where("adapter_type <> ?", "mock").Count(&realProviders).Error; err != nil {
		return fmt.Errorf("demo seed: check providers: %w", err)
	}
	if realProviders > 0 {
		return errors.New("demo mode refused: a non-mock provider already exists")
	}

	keyHash := cp.HashHex([]byte(demoAPIKey))
	prefix := demoKeyPrefix
	hashAlgo := string(cp.Algorithms().Hash)
	allowedModels, _ := json.Marshal(demoModels)
	allowedRoutes, _ := json.Marshal([]string{"anthropic", "openai"})

	return db.Transaction(func(tx *gorm.DB) error {
		// Provider (idempotent by name)
		var provider model.Provider
		err := tx.Where("name = ?", demoProviderName).First(&provider).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			provider = model.Provider{
				Name:        demoProviderName,
				DisplayName: "Mock (Demo)",
				AdapterType: "mock",
				BaseURL:     "", // mock needs none
				APIKey:      "mock", // column is NOT NULL; mock adapter ignores it
				Status:      1,
			}
			if err := tx.Create(&provider).Error; err != nil {
				return fmt.Errorf("demo seed: create provider: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("demo seed: query provider: %w", err)
		}

		// Models (idempotent: skip names that already exist under this provider)
		for _, mName := range demoModels {
			var cnt int64
			if err := tx.Model(&model.ProviderModel{}).Where("provider_id = ? AND model_name = ?", provider.ID, mName).Count(&cnt).Error; err != nil {
				return fmt.Errorf("demo seed: check model: %w", err)
			}
			if cnt > 0 {
				continue
			}
			m := model.ProviderModel{
				ProviderID:    provider.ID,
				ModelName:     mName,
				ProviderModel: mName,
				Status:        1,
			}
			if err := tx.Create(&m).Error; err != nil {
				return fmt.Errorf("demo seed: create model: %w", err)
			}
		}

		// API key + hash (idempotent by key_prefix)
		var existingKey model.APIKey
		err = tx.Where("key_prefix = ?", prefix).First(&existingKey).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			key := model.APIKey{
				Name:          "demo (mock-only)",
				KeyHash:       keyHash,
				KeyPrefix:     prefix,
				Status:        1,
				AllowedModels: allowedModels,
				AllowedRoutes: allowedRoutes,
				CreatedBy:     "demo",
			}
			if err := tx.Create(&key).Error; err != nil {
				return fmt.Errorf("demo seed: create api key: %w", err)
			}
			hashRecord := model.APIKeyHash{
				KeyHash:   keyHash,
				KeyPrefix: prefix,
				HashAlgo:  hashAlgo,
				IsPrimary: true,
				APIKeyID:  key.ID,
			}
			if err := tx.Create(&hashRecord).Error; err != nil {
				return fmt.Errorf("demo seed: create hash record: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("demo seed: query api key: %w", err)
		}
		return nil
	})
}
