package license

import (
	"strings"
	"testing"
)

// TestConfigureOffIgnoredInProductionBuild pins the production posture
// (devEscapeHatchAllowed=false, regardless of the compiled-in value) and
// asserts the security property the build gate exists for:
//
//	configure(verify_mode=off) → NO error, but verification stays ACTIVE —
//	mode != off, the offIgnoredNote is recorded, and a forged (unsigned /
//	wrong-key) token is REJECTED.
//
// This is the regression test for the "dev compose copied to prod +
// self-issued token" attack chain: in a production build that chain must be
// inert even though the operator explicitly asked for off.
func TestConfigureOffIgnoredInProductionBuild(t *testing.T) {
	orig := devEscapeHatchAllowed
	devEscapeHatchAllowed = false
	t.Cleanup(func() { devEscapeHatchAllowed = orig })
	t.Cleanup(func() { _ = ConfigureSignatureVerification(VerifyModeAuto, "", "", "") })

	whitelisted := newRS256(t)
	attacker := newRS256(t)

	// off requested WITHOUT keys (the dev-compose-copied-to-prod shape):
	// degrades to keyless auto — enforce + note, every token rejected.
	if err := ConfigureSignatureVerification(VerifyModeOff, "", "", ""); err != nil {
		t.Fatalf("off in production build must not be a configure error, got %v", err)
	}
	if VerificationMode() == VerifyModeOff {
		t.Fatal("production build must not honor verify_mode=off")
	}
	if VerificationMode() != VerifyModeEnforce {
		t.Fatalf("off must degrade to enforce, got %q", VerificationMode())
	}
	if !strings.Contains(VerificationNote(), "off was IGNORED") {
		t.Fatalf("off degradation must be explained by the note, got %q", VerificationNote())
	}
	if err := VerifyRawToken(mint(t, attacker, "k1", stdClaims())); err == nil {
		t.Fatal("forged token must be REJECTED when off is ignored (keyless auto)")
	}

	// off requested WITH trusted keys: valid signatures still load, the
	// forged one does not.
	spec := "{" + kidJSON("k1", whitelisted.material()) + "}"
	if err := ConfigureSignatureVerification(VerifyModeOff, "", "", spec); err != nil {
		t.Fatalf("off+keys configure: %v", err)
	}
	if VerificationMode() != VerifyModeEnforce {
		t.Fatalf("off+keys must degrade to enforce, got %q", VerificationMode())
	}
	if err := VerifyRawToken(mint(t, whitelisted, "k1", stdClaims())); err != nil {
		t.Fatalf("valid signed token must still pass: %v", err)
	}
	if err := VerifyRawToken(mint(t, attacker, "k1", stdClaims())); err == nil {
		t.Fatal("forged token must be rejected even though off was requested")
	}
}
