package configio

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	"gorm.io/gorm"
)

// ApplyReport summarizes an import run. Skipped entries are name-level conflicts
// (already exist); Errors are per-record failures (resolve/parse/lookup) that did
// not abort the run. If Aborted is true, a non-recoverable DB error stopped the
// import partway — Created/Skipped/Errors reflect progress up to that point.
type ApplyReport struct {
	DryRun  bool
	Created struct {
		Providers int
		Models    int
		ErrorRules int
	}
	Skipped []SkippedEntry
	Errors  []string
	Aborted string // empty unless a fatal error aborted the batch
}

type SkippedEntry struct {
	Type   string // "provider" | "model" | "error_rule"
	Name   string
	Reason string
}

// ApplyBundle imports the bundle into db. On conflict (same name / same dedup
// key) the existing record is kept and the incoming one is skipped. Per-record
// failures (resolve errors, missing provider_name lookup) are recorded and the
// run continues; a non-recoverable DB connection error aborts the batch.
//
// Each insert is independent (no wrapping transaction) so a large bundle does
// not hold a long-running transaction. Provider name is unique-indexed so
// repeated imports are idempotent at the provider level; model/error_rule
// dedup is a soft check-then-insert (idempotent for single-process CLI use).
func ApplyBundle(db *gorm.DB, encStore *secret.EncryptedDBStore, bundle *ExportBundle, dryRun bool) (*ApplyReport, error) {
	report := &ApplyReport{DryRun: dryRun}

	// 1) Providers first, building a name -> id map for model association.
	providerIDByName := make(map[string]int64, len(bundle.Providers))
	for i := range bundle.Providers {
		pe := &bundle.Providers[i]

		// Dedup: existing provider with same name?
		var existing model.Provider
		result := db.Where("name = ?", pe.Name).First(&existing)
		if result.Error == nil {
			providerIDByName[pe.Name] = existing.ID
			report.Skipped = append(report.Skipped, SkippedEntry{Type: "provider", Name: pe.Name, Reason: "name exists"})
			continue
		} else if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			// Non-recoverable DB error — abort.
			report.Aborted = fmt.Sprintf("query provider %q: %v", pe.Name, result.Error)
			return report, nil
		}

		apiKey, err := resolveForImport(encStore, pe.APIKey)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("provider %q api_key: %v", pe.Name, err))
			continue
		}
		extra, err := resolveExtraConfigForImport(encStore, pe.ExtraConfig)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("provider %q extra_config: %v", pe.Name, err))
			continue
		}

		p := &model.Provider{
			Name: pe.Name, DisplayName: pe.DisplayName, AdapterType: pe.AdapterType,
			BaseURL: pe.BaseURL, APIKey: apiKey, ExtraConfig: extra,
			Status: pe.Status,
		}
		if !dryRun {
			if err := db.Create(p).Error; err != nil {
				// Constraint violation -> skip; connection error -> abort.
				if isConstraintErr(err) {
					report.Skipped = append(report.Skipped, SkippedEntry{Type: "provider", Name: pe.Name, Reason: err.Error()})
					continue
				}
				report.Aborted = fmt.Sprintf("create provider %q: %v", pe.Name, err)
				return report, nil
			}
			providerIDByName[pe.Name] = p.ID
		}
		report.Created.Providers++
		providerIDByName[pe.Name] = providerIDByName[pe.Name] // dry-run: id stays 0, models will lookup-fail gracefully
	}

	// 2) Models — resolve provider_id by name, soft-dedup on (provider_id, model_name).
	for i := range bundle.Models {
		me := &bundle.Models[i]
		_, ok := providerIDByName[me.ProviderName]
		if !ok {
			report.Errors = append(report.Errors, fmt.Sprintf("model %q: provider %q not found (skipped or absent)", me.ModelName, me.ProviderName))
			continue
		}
		if dryRun {
			// No real provider id (nothing persisted); skip dedup query and count
			// the model as would-create so dry-run totals reflect the real run.
			report.Created.Models++
			continue
		}
		providerID := providerIDByName[me.ProviderName]

		// Soft dedup: existing (provider_id, model_name)?
		var cnt int64
		if err := db.Model(&model.ProviderModel{}).Where("provider_id = ? AND model_name = ?", providerID, me.ModelName).Count(&cnt).Error; err != nil {
			if isConnectionErr(err) {
				report.Aborted = fmt.Sprintf("count model %q: %v", me.ModelName, err)
				return report, nil
			}
			report.Errors = append(report.Errors, fmt.Sprintf("model %q dedup query: %v", me.ModelName, err))
			continue
		}
		if cnt > 0 {
			report.Skipped = append(report.Skipped, SkippedEntry{Type: "model", Name: fmt.Sprintf("%s/%s", me.ProviderName, me.ModelName), Reason: "exists"})
			continue
		}

		if !dryRun {
			m := &model.ProviderModel{
				ProviderID: providerID, ModelName: me.ModelName, ProviderModel: me.ProviderModel,
				Weight: me.Weight, Priority: me.Priority, InputPrice: me.InputPrice,
				OutputPrice: me.OutputPrice, Currency: me.Currency,
				RoutingStrategy: me.RoutingStrategy, Status: me.Status,
			}
			if err := db.Create(m).Error; err != nil {
				if isConstraintErr(err) {
					report.Skipped = append(report.Skipped, SkippedEntry{Type: "model", Name: me.ModelName, Reason: err.Error()})
					continue
				}
				report.Errors = append(report.Errors, fmt.Sprintf("create model %q: %v", me.ModelName, err))
				continue
			}
		}
		report.Created.Models++
	}

	// 3) ErrorClassificationRules — dedup on (match_field, pattern, scope, provider_type).
	for i := range bundle.ErrorRules {
		re := &bundle.ErrorRules[i]
		q := db.Model(&model.ErrorClassificationRule{}).
			Where("match_field = ? AND pattern = ? AND scope = ?", re.MatchField, re.Pattern, re.Scope)
		if re.ProviderType == nil || *re.ProviderType == "" {
			q = q.Where("provider_type IS NULL OR provider_type = ''")
		} else {
			q = q.Where("provider_type = ?", *re.ProviderType)
		}
		var cnt int64
		if err := q.Count(&cnt).Error; err != nil {
			if isConnectionErr(err) {
				report.Aborted = fmt.Sprintf("count error_rule: %v", err)
				return report, nil
			}
			report.Errors = append(report.Errors, fmt.Sprintf("error_rule dedup query: %v", err))
			continue
		}
		if cnt > 0 {
			report.Skipped = append(report.Skipped, SkippedEntry{Type: "error_rule", Name: fmt.Sprintf("%s/%s", re.MatchField, re.Pattern), Reason: "exists"})
			continue
		}
		if !dryRun {
			r := &model.ErrorClassificationRule{
				MatchField: re.MatchField, Pattern: re.Pattern, Classification: re.Classification,
				ProviderType: re.ProviderType, Scope: re.Scope, Priority: re.Priority, Enabled: re.Enabled,
			}
			if err := db.Create(r).Error; err != nil {
				if isConstraintErr(err) {
					report.Skipped = append(report.Skipped, SkippedEntry{Type: "error_rule", Name: re.Pattern, Reason: err.Error()})
					continue
				}
				report.Errors = append(report.Errors, fmt.Sprintf("create error_rule %q: %v", re.Pattern, err))
				continue
			}
		}
		report.Created.ErrorRules++
	}

	return report, nil
}

// isConstraintErr reports whether err is a unique/constraint violation, which we
// treat as a skip (conflict) rather than abort. Covers Postgres, SQLite, MySQL.
// SQLSTATE 23505 (unique_violation) is matched first because it is locale-
// independent — Postgres appends it regardless of message language (catches
// the zh_CN "重复键违反唯一约束" form).
func isConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "23505", "duplicate key", "Duplicate entry", "UNIQUE constraint", "uniqueIndex", "constraint failed", "重复键")
}

// isConnectionErr reports whether err indicates a lost DB connection (abort).
func isConnectionErr(err error) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), "connection", "EOF", "broken pipe", "timeout", "reset")
}

func contains(haystack string, needles ...string) bool {
	lower := strings.ToLower(haystack)
	for _, n := range needles {
		if strings.Contains(lower, strings.ToLower(n)) {
			return true
		}
	}
	return false
}
