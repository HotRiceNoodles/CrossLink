package config

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/crosslink/internal/model"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

type ProviderSeed struct {
	Providers []ProviderConfig `yaml:"providers"`
}

type ProviderConfig struct {
	Name        string        `yaml:"name"`
	DisplayName string        `yaml:"display_name"`
	AdapterType string        `yaml:"adapter_type"`
	BaseURL     string        `yaml:"base_url"`
	APIKey      string        `yaml:"api_key"`
	Models      []ModelConfig `yaml:"models"`
}

type ModelConfig struct {
	ModelName     string  `yaml:"model_name"`
	ProviderModel string  `yaml:"provider_model"`
	Weight        int     `yaml:"weight"`
	Priority      int     `yaml:"priority"`
	InputPrice    float64 `yaml:"input_price"`
	OutputPrice   float64 `yaml:"output_price"`
	Currency      string  `yaml:"currency"`
}

func SeedProviders(db *gorm.DB, path string) error {
	var count int64
	db.Table("providers").Count(&count)
	if count > 0 {
		slog.Info("providers already exist, skipping seed")
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read providers config: %w", err)
	}

	var seed ProviderSeed
	if err := yaml.Unmarshal(data, &seed); err != nil {
		return fmt.Errorf("parse providers config: %w", err)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, p := range seed.Providers {
			apiKey := os.ExpandEnv(p.APIKey)
			provider := model.Provider{
				Name:        p.Name,
				DisplayName: p.DisplayName,
				AdapterType: p.AdapterType,
				BaseURL:     p.BaseURL,
				APIKey:      apiKey,
				Status:      1,
			}
			if err := tx.Create(&provider).Error; err != nil {
				return fmt.Errorf("create provider %s: %w", p.Name, err)
			}

			for _, m := range p.Models {
				pm := model.ProviderModel{
					ProviderID:    provider.ID,
					ModelName:     m.ModelName,
					ProviderModel: m.ProviderModel,
					Weight:        m.Weight,
					Priority:      m.Priority,
					InputPrice:    m.InputPrice,
					OutputPrice:   m.OutputPrice,
					Currency:      m.Currency,
					Status:        1,
				}
				if err := tx.Create(&pm).Error; err != nil {
					return fmt.Errorf("create model %s: %w", m.ModelName, err)
				}
			}
		}
		slog.Info("seeded providers from config", "count", len(seed.Providers))
		return nil
	})
}
