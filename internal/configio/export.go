package configio

import (
	"fmt"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	"gorm.io/gorm"
)

// BuildBundle reads configuration entities from db and assembles an ExportBundle.
// encStore is used to decrypt enc://-style api_key / extra_config secret fields;
// it may be nil for plaintext deployments (in which case enc:// values produce
// a per-provider error and that provider is omitted from the bundle).
func BuildBundle(db *gorm.DB, encStore *secret.EncryptedDBStore) (*ExportBundle, []error, error) {
	bundle := &ExportBundle{
		Version:    BundleVersion,
		ExportedAt: time.Now().UTC(),
	}

	// Providers
	var providers []model.Provider
	if err := db.Find(&providers).Error; err != nil {
		return nil, nil, fmt.Errorf("load providers: %w", err)
	}

	// Index models by provider_id for grouping.
	var allModels []model.ProviderModel
	if err := db.Find(&allModels).Error; err != nil {
		return nil, nil, fmt.Errorf("load provider_models: %w", err)
	}
	modelsByProvider := make(map[int64][]model.ProviderModel)
	for i := range allModels {
		modelsByProvider[allModels[i].ProviderID] = append(modelsByProvider[allModels[i].ProviderID], allModels[i])
	}

	var warns []error
	for i := range providers {
		p := &providers[i]
		pe := ProviderExport{
			Name:        p.Name,
			DisplayName: p.DisplayName,
			AdapterType: p.AdapterType,
			BaseURL:     p.BaseURL,
			Status:      p.Status,
		}
		apiKey, err := resolveForExport(encStore, p.APIKey)
		if err != nil {
			warns = append(warns, fmt.Errorf("provider %q: %w", p.Name, err))
			continue // skip this provider (and its models) — secret not recoverable
		}
		pe.APIKey = apiKey
		extra, err := resolveExtraConfigForExport(encStore, p.ExtraConfig)
		if err != nil {
			warns = append(warns, fmt.Errorf("provider %q extra_config: %w", p.Name, err))
			continue
		}
		pe.ExtraConfig = extra
		bundle.Providers = append(bundle.Providers, pe)

		for _, m := range modelsByProvider[p.ID] {
			bundle.Models = append(bundle.Models, ModelExport{
				ProviderName:    p.Name,
				ModelName:       m.ModelName,
				ProviderModel:   m.ProviderModel,
				Weight:          m.Weight,
				Priority:        m.Priority,
				InputPrice:      m.InputPrice,
				OutputPrice:     m.OutputPrice,
				Currency:        m.Currency,
				RoutingStrategy: m.RoutingStrategy,
				Status:          m.Status,
			})
		}
	}

	// ErrorClassificationRules (global)
	var rules []model.ErrorClassificationRule
	if err := db.Find(&rules).Error; err != nil {
		return nil, nil, fmt.Errorf("load error_rules: %w", err)
	}
	for i := range rules {
		r := &rules[i]
		bundle.ErrorRules = append(bundle.ErrorRules, ErrorRuleExport{
			MatchField:     r.MatchField,
			Pattern:        r.Pattern,
			Classification: r.Classification,
			ProviderType:   r.ProviderType,
			Scope:          r.Scope,
			Priority:       r.Priority,
			Enabled:        r.Enabled,
		})
	}

	return bundle, warns, nil
}
