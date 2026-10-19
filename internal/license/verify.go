package license

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// Ed25519 license-token signature verification (U-37; user-authorized
// 2026-10-19). Production signing keys are held by the USER — this tree
// ships and generates NONE; tests create ephemeral ISOLATED TEST KEYS only.
// Modes: off (default — no check, no enforcement), warn (verify + warn on
// failure, token still accepted), enforce (verify; an invalid token is
// rejected on EVERY load path: offline file, heartbeat cache, DB). The
// enforcement date is the user's call; the default is OFF and must not be
// flipped unilaterally.
//
// Checks performed in warn/enforce:
//   - alg is EXACTLY EdDSA/Ed25519 — any other algorithm (HS256 with an
//     attacker-chosen key, "none", RS256) is rejected BEFORE any key
//     material is consulted: no algorithm-confusion window;
//   - the token header carries a kid present in the configured whitelist
//     (rotation = ship multiple kids; revocation = remove from the list);
//   - the signature verifies under that kid's Ed25519 public key;
//   - exp present and valid; iss/aud match when configured;
//   - the pre-existing fingerprint binding and grace/expiry logic stay at
//     their call sites (verification composes with, not replaces, them).

const (
	VerifyModeOff     = "off"
	VerifyModeWarn    = "warn"
	VerifyModeEnforce = "enforce"
)

type tokenVerification struct {
	mode     string
	issuer   string
	audience string
	keys     map[string]ed25519.PublicKey
}

var verification tokenVerification

// ConfigureError reports invalid verification configuration.
type ConfigureError struct{ Reason string }

func (e *ConfigureError) Error() string { return "license verification configure: " + e.Reason }

// ConfigureSignatureVerification installs the global verification state.
// spec is inline key material or "@<path>". Key material forms:
//   - JSON object { "<kid>": "<base64 raw 32-byte public key>", ... }
//   - PEM PUBLIC KEY list; a block may be preceded by a "# kid=<id>" line
//     (a block without one registers under kid "")
func ConfigureSignatureVerification(mode, issuer, audience, spec string) error {
	switch mode {
	case "", VerifyModeOff:
		mode = VerifyModeOff
	case VerifyModeWarn, VerifyModeEnforce:
	default:
		return &ConfigureError{Reason: fmt.Sprintf("unknown mode %q (off|warn|enforce)", mode)}
	}
	keys, err := loadEd25519PublicKeys(spec)
	if err != nil {
		return err
	}
	if mode != VerifyModeOff && len(keys) == 0 {
		return &ConfigureError{Reason: "mode " + mode + " requires at least one whitelisted public key"}
	}
	verification = tokenVerification{mode: mode, issuer: issuer, audience: audience, keys: keys}
	return nil
}

// VerificationMode reports the active mode (boot logs and tests).
func VerificationMode() string { return verification.mode }

func loadEd25519PublicKeys(spec string) (map[string]ed25519.PublicKey, error) {
	out := map[string]ed25519.PublicKey{}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return out, nil
	}
	if strings.HasPrefix(spec, "@") {
		b, err := os.ReadFile(strings.TrimPrefix(spec, "@"))
		if err != nil {
			return nil, &ConfigureError{Reason: "read key file: " + err.Error()}
		}
		spec = string(b)
	}
	if trimmed := strings.TrimSpace(spec); strings.HasPrefix(trimmed, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
			return nil, &ConfigureError{Reason: "JSON key map: " + err.Error()}
		}
		for kid, b64 := range m {
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil || len(raw) != ed25519.PublicKeySize {
				return nil, &ConfigureError{Reason: fmt.Sprintf("kid %q: want base64 %d-byte raw key", kid, ed25519.PublicKeySize)}
			}
			out[kid] = ed25519.PublicKey(raw)
		}
		return out, nil
	}
	// PEM list. A "# kid=<id>" line attaches the NEXT block.
	lines := strings.Split(spec, "\n")
	pendingKid := ""
	var block strings.Builder
	flush := func() error {
		if block.Len() == 0 {
			return nil
		}
		blk, _ := pem.Decode([]byte(block.String()))
		block.Reset()
		if blk == nil {
			return &ConfigureError{Reason: "unreadable PUBLIC KEY PEM block"}
		}
		pub, err := parseEd25519PEM(blk)
		if err != nil {
			return &ConfigureError{Reason: err.Error()}
		}
		out[pendingKid] = pub
		pendingKid = ""
		return nil
	}
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "# kid=") {
			pendingKid = strings.TrimSpace(strings.TrimPrefix(trimmed, "# kid="))
			continue
		}
		if strings.HasPrefix(trimmed, "-----") {
			block.WriteString(ln + "\n")
			if strings.HasPrefix(trimmed, "-----END") {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			continue
		}
		if block.Len() > 0 {
			block.WriteString(ln + "\n")
		}
	}
	if block.Len() > 0 {
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, &ConfigureError{Reason: "no usable PUBLIC KEY PEM blocks found"}
	}
	return out, nil
}

func parseEd25519PEM(block *pem.Block) (ed25519.PublicKey, error) {
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("unsupported PEM block %q (want PUBLIC KEY)", block.Type)
	}
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if ed, ok := k.(ed25519.PublicKey); ok {
			return ed, nil
		}
		return nil, errors.New("PKIX key is not Ed25519")
	}
	if len(block.Bytes) == ed25519.PublicKeySize {
		return ed25519.PublicKey(block.Bytes), nil
	}
	return nil, errors.New("not an Ed25519 public key (PKIX or raw 32-byte)")
}

// VerifyRawToken runs the full signature/claims verification. Returns nil
// when valid, or when verification is OFF (no checks run in that mode).
func VerifyRawToken(rawToken string) error {
	v := verification
	if v.mode == VerifyModeOff {
		return nil
	}
	if len(v.keys) == 0 {
		return errors.New("verification configured without keys")
	}
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithExpirationRequired(),
	}
	if v.issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.issuer))
	}
	if v.audience != "" {
		opts = append(opts, jwt.WithAudience(v.audience))
	}
	parsed, err := jwt.Parse(rawToken, func(t *jwt.Token) (interface{}, error) {
		// Exact algorithm match BEFORE any key material is consulted.
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("algorithm %q not allowed (EdDSA/Ed25519 only)", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		key, ok := v.keys[kid]
		if !ok {
			return nil, fmt.Errorf("kid %q not in whitelist", kid)
		}
		return key, nil
	}, opts...)
	if err != nil {
		return err
	}
	if !parsed.Valid {
		return errors.New("token invalid")
	}
	return nil
}
