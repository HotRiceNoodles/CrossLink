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

// configureVerificationFromConfig installs the U-37 Ed25519 verification
// state from config. A malformed configuration is a hard startup error in
// warn/enforce (fail-closed on misconfiguration), while mode off is the
// silent default.
func configureVerificationFromConfig(cfg config.LicenseConfig) {
	if err := ConfigureSignatureVerification(cfg.VerifyMode, cfg.VerifyIssuer, cfg.VerifyAudience, cfg.VerifyKeys); err != nil {
		slog.Error("license signature verification MISCONFIGURED — falling back to off; fix config before enabling",
			"error", err)
		_ = ConfigureSignatureVerification(VerifyModeOff, "", "", "")
		return
	}
	if VerificationMode() != VerifyModeOff {
		slog.Info("license signature verification active",
			"mode", VerificationMode(), "keys", lenVerificationKeys())
	}
}

func lenVerificationKeys() int { return len(verification.keys) }
