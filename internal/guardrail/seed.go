package guardrail

import (
	"fmt"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// SeedDefaultRules installs a sensible zero-config baseline of guardrail rules.
//
// Currently this is a single "credential_detection" rule that runs on both
// request and response, across all models, in log-only mode. log (not block) is
// deliberate: a default rule must not disrupt legit traffic with false
// positives — it demonstrates the feature safely and admins can switch the
// action to block/mask via the UI once they trust the detection.
//
// Idempotent: if any credential_detection rule already exists (this default or
// an operator-created one), nothing is inserted. Safe to call on every startup.
func SeedDefaultRules(db *gorm.DB) error {
	var n int64
	if err := db.Model(&GuardrailRule{}).
		Where("type = ?", "credential_detection").
		Count(&n).Error; err != nil {
		return fmt.Errorf("guardrail seed: check existing: %w", err)
	}
	if n > 0 {
		return nil
	}

	// Config is intentionally empty ({}): when categories is absent/empty, the
	// credential_detection engine derives ALL builtin categories at runtime
	// (credential_detection.go:102-106). This keeps the engine as the single
	// source of truth — adding a new builtin category no longer requires
	// updating the seed.
	rule := GuardrailRule{
		Name:      "Default - Credential Leak Detection",
		Type:      "credential_detection",
		Direction: "both",
		Enabled:   true,
		Severity:  "medium",
		Action:    "log",
		Config:    datatypes.JSON([]byte("{}")),
		// OrgID and ModelFilter intentionally nil: global default, all models.
	}
	if err := db.Create(&rule).Error; err != nil {
		return fmt.Errorf("guardrail seed: insert default rule: %w", err)
	}
	return nil
}
