package license

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	gmSm2 "github.com/tjfoc/gmsm/sm2"
	smx509 "github.com/tjfoc/gmsm/x509"
)

// License-token signature verification (U-37; user-authorized 2026-10-19,
// reworked the same day per review to REUSE the existing issuance trust
// chain instead of an incompatible Ed25519-only scheme).
//
// The issuer (License-Server, audited read-only) signs with EITHER
//   RS256   — RSA-2048, SHA-256, PKCS#1 v1.5 (internal/crypto/standard.go)
//   SM2SM3  — national-cryptography provider (internal/crypto/gm_jwt.go)
// and addresses keys by a `kid` header; rotation = new SigningKey rows
// (RotateKey) with public PEMs exposed by the admin signing-key endpoint.
// This verifier MIRRORS the server's byte-level verification semantics
// (same signing-string, same primitives) so a token the server accepts,
// the client accepts — and nothing else:
//   - the header alg must be exactly RS256 / SM2SM3 / EdDSA (EdDSA kept as
//     an OPTIONAL future path — enabling it issuer-side requires the
//     two-sided migration plan in decision-packages §B; never unilateral);
//   - kid must resolve in the configured whitelist (JSON {kid: PEM|b64raw}
//     or PEM list with "# kid=" lines; rotation = add entries, revocation
//     = remove);
//   - signature verified under that kid's key; exp required and checked;
//     iss/aud checked when configured;
//   - the pre-existing fingerprint binding / grace logic at the client call
//     sites composes unchanged.
// Modes: off (default) | warn (log failures, accept) | enforce (reject on
// every load path — the single ParseToken point covers offline/cache/DB).
// Production keys are the licensee's; this tree ships and generates NONE.

const (
	VerifyModeOff     = "off"
	VerifyModeWarn    = "warn"
	VerifyModeEnforce = "enforce"
)

type licensedKey struct {
	alg string // RS256 | SM2SM3 | EdDSA
	rsa *rsa.PublicKey
	sm2 *gmSm2.PublicKey
	ed  ed25519.PublicKey
}

type tokenVerification struct {
	mode     string
	issuer   string
	audience string
	keys     map[string]*licensedKey
}

var verification tokenVerification

// ConfigureError reports invalid verification configuration.
type ConfigureError struct{ Reason string }

func (e *ConfigureError) Error() string { return "license verification configure: " + e.Reason }

// ConfigureSignatureVerification installs the global verification state.
// spec is inline key material or "@<path>": JSON {kid: <PEM text or base64
// raw Ed25519 key>} or a PEM list where "# kid=<id>" precedes its block.
func ConfigureSignatureVerification(mode, issuer, audience, spec string) error {
	switch mode {
	case "", VerifyModeOff:
		mode = VerifyModeOff
	case VerifyModeWarn, VerifyModeEnforce:
	default:
		return &ConfigureError{Reason: fmt.Sprintf("unknown mode %q (off|warn|enforce)", mode)}
	}
	keys, err := loadLicenseKeys(spec)
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

func loadLicenseKeys(spec string) (map[string]*licensedKey, error) {
	out := map[string]*licensedKey{}
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
		for kid, material := range m {
			k, err := parseKeyMaterial(material)
			if err != nil {
				return nil, &ConfigureError{Reason: fmt.Sprintf("kid %q: %v", kid, err)}
			}
			out[kid] = k
		}
		return out, nil
	}
	// PEM list; "# kid=<id>" attaches the next block.
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
		k, err := parsePEMBlock(blk)
		if err != nil {
			return &ConfigureError{Reason: err.Error()}
		}
		out[pendingKid] = k
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
		return nil, &ConfigureError{Reason: "no usable public-key PEM blocks found"}
	}
	return out, nil
}

func parseKeyMaterial(material string) (*licensedKey, error) {
	trimmed := strings.TrimSpace(material)
	if blk, _ := pem.Decode([]byte(trimmed)); blk != nil {
		return parsePEMBlock(blk)
	}
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("want a PEM public key or base64 raw Ed25519 key")
	}
	return &licensedKey{alg: "EdDSA", ed: ed25519.PublicKey(raw)}, nil
}

// parseSM2Block mirrors the server's parseSM2PublicKey (internal/crypto/gm.go):
// pem.Decode then smx509.ParseSm2PublicKey on the DER, regardless of the PEM
// type label — the server WRITES "SM2 PUBLIC KEY" (gm.go:120) which tjfoc's
// own ReadPublicKeyFromPem rejects (it demands "PUBLIC KEY"), so the type
// must not gate the parse.
func parseSM2Block(blk *pem.Block) (*gmSm2.PublicKey, error) {
	return smx509.ParseSm2PublicKey(blk.Bytes)
}

func parsePEMBlock(blk *pem.Block) (*licensedKey, error) {
	if strings.Contains(blk.Type, "RSA") {
		if k, err := x509.ParsePKIXPublicKey(blk.Bytes); err == nil {
			if rk, ok := k.(*rsa.PublicKey); ok {
				return &licensedKey{alg: "RS256", rsa: rk}, nil
			}
		}
		if rk, err := x509.ParsePKCS1PublicKey(blk.Bytes); err == nil {
			return &licensedKey{alg: "RS256", rsa: rk}, nil
		}
		return nil, errors.New("not an RSA public key")
	}
	if strings.Contains(blk.Type, "SM2") {
		pub, err := parseSM2Block(blk)
		if err != nil {
			return nil, errors.New("not an SM2 public key: " + err.Error())
		}
		return &licensedKey{alg: "SM2SM3", sm2: pub}, nil
	}
	if blk.Type == "PUBLIC KEY" {
		if k, err := x509.ParsePKIXPublicKey(blk.Bytes); err == nil {
			switch pk := k.(type) {
			case *rsa.PublicKey:
				return &licensedKey{alg: "RS256", rsa: pk}, nil
			case ed25519.PublicKey:
				return &licensedKey{alg: "EdDSA", ed: pk}, nil
			}
		}
		if len(blk.Bytes) == ed25519.PublicKeySize {
			return &licensedKey{alg: "EdDSA", ed: ed25519.PublicKey(blk.Bytes)}, nil
		}
		// Generic PKIX block carrying an SM2 key (tjfoc's own writer emits
		// "PUBLIC KEY"); fall through to the server-mirrored SM2 parse.
		if pub, err := parseSM2Block(blk); err == nil {
			return &licensedKey{alg: "SM2SM3", sm2: pub}, nil
		}
		return nil, errors.New("unsupported PUBLIC KEY block (want RSA, Ed25519 or SM2)")
	}
	if strings.Contains(blk.Type, "EC") {
		// SM2 keys are EC-shaped; try the server-mirrored SM2 parse.
		if pub, err := parseSM2Block(blk); err == nil {
			return &licensedKey{alg: "SM2SM3", sm2: pub}, nil
		}
		return nil, errors.New("unsupported EC key (not SM2)")
	}
	return nil, fmt.Errorf("unsupported PEM block %q", blk.Type)
}

// VerifyRawToken verifies a raw license JWT against the whitelist using the
// ISSUER's exact semantics. Returns nil when valid, or when verification is
// OFF (no checks run in that mode).
func VerifyRawToken(rawToken string) error {
	v := verification
	if v.mode == VerifyModeOff {
		return nil
	}
	if len(v.keys) == 0 {
		return errors.New("verification configured without keys")
	}
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return errors.New("malformed JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("decode header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return fmt.Errorf("parse header: %w", err)
	}
	switch header.Alg {
	case "RS256", "SM2SM3", "EdDSA":
	default:
		return fmt.Errorf("algorithm %q not allowed (RS256/SM2SM3/EdDSA only)", header.Alg)
	}
	key, ok := v.keys[header.Kid]
	if !ok {
		return fmt.Errorf("kid %q not in whitelist", header.Kid)
	}
	if key.alg != header.Alg {
		return fmt.Errorf("kid %q holds an %s key, token alg is %s", header.Kid, key.alg, header.Alg)
	}
	signingString := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	switch header.Alg {
	case "RS256":
		hash := sha256.Sum256([]byte(signingString))
		if err := rsa.VerifyPKCS1v15(key.rsa, crypto.SHA256, hash[:], sig); err != nil {
			return fmt.Errorf("RS256 signature invalid: %w", err)
		}
	case "SM2SM3":
		if !key.sm2.Verify([]byte(signingString), sig) {
			return errors.New("SM2SM3 signature invalid")
		}
	case "EdDSA":
		if !ed25519.Verify(ed25519.PublicKey(key.ed), []byte(signingString), sig) {
			return errors.New("EdDSA signature invalid")
		}
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("decode claims: %w", err)
	}
	var claims struct {
		Exp float64 `json:"exp"`
		Iss string  `json:"iss"`
		Aud string  `json:"aud"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		return fmt.Errorf("parse claims: %w", err)
	}
	if claims.Exp == 0 {
		return errors.New("token missing exp")
	}
	if time.Now().Unix() > int64(claims.Exp) {
		return errors.New("token expired")
	}
	if v.issuer != "" && claims.Iss != v.issuer {
		return fmt.Errorf("issuer %q != expected %q", claims.Iss, v.issuer)
	}
	if v.audience != "" && claims.Aud != v.audience {
		return fmt.Errorf("audience %q != expected %q", claims.Aud, v.audience)
	}
	return nil
}
