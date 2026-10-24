package license

import (
	"log/slog"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"gorm.io/gorm"
)

// CommunityInit creates a Community-mode Gate without license server communication.
// The cp parameter is accepted for signature parity with Init() but is not used.
func CommunityInit(cfg config.LicenseConfig, db *gorm.DB, cp crypto.CryptoProvider) *Gate {
	configureVerificationFromConfig(cfg)
	g := CommunityGate()
	global = g
	return g
}

// configureVerificationFromConfig installs the U-37 verification state from
// config. FAIL-CLOSED everywhere (security review 2026-10-24):
//   - misconfiguration (unknown mode, unusable key spec, keyless
//     warn/enforce) → lockdown: every token rejected until the config is
//     fixed; licensed tiers degrade loudly to Community, the process keeps
//     serving;
//   - the auto default without keys → every licensed token rejected with a
//     clear reason (Community unaffected) — deployments upgrade by
//     configuring keys (pre-upgrade checklist in
//     docs/节点身份与授权迁移.md), never by relaxing verification;
//   - off is honored only as an EXPLICIT development choice and is called
//     out as such in the boot log.
func configureVerificationFromConfig(cfg config.LicenseConfig) {
	if err := ConfigureSignatureVerification(cfg.VerifyMode, cfg.VerifyIssuer, cfg.VerifyAudience, cfg.VerifyKeys); err != nil {
		requested := cfg.VerifyMode
		if requested == "" {
			requested = VerifyModeAuto
		}
		lockdownVerification(requested, err.Error())
		slog.Error("license signature verification MISCONFIGURED — locked down, every license token will be rejected until the config is fixed",
			"requested", requested, "error", err)
		return
	}
	if note := VerificationNote(); note != "" {
		slog.Error("license verification unavailable — licensed tokens are REJECTED and the node runs as Community",
			"configured", VerificationConfiguredMode(), "note", note)
		return
	}
	if VerificationMode() == VerifyModeOff {
		slog.Warn("license signature verification is EXPLICITLY OFF (development only) — license tokens are accepted WITHOUT signature checks")
		return
	}
	slog.Info("license signature verification active",
		"mode", VerificationMode(), "configured", VerificationConfiguredMode(), "keys", lenVerificationKeys())
}

func lenVerificationKeys() int { return len(verification.keys) }
