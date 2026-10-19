package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// U-37 Ed25519 verification tests. ALL keys here are EPHEMERAL TEST KEYS
// generated in-process — this suite never ships, writes, or discloses any
// production key material (production signing keys are held by the user).

type testKeys struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newTestKey(t *testing.T) testKeys {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return testKeys{priv: priv, pub: pub}
}

func (k testKeys) sign(t *testing.T, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(k.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func keyJSON(k testKeys) string {
	return `{"k1":"` + base64.StdEncoding.EncodeToString(k.pub) + `"}`
}

func setMode(t *testing.T, mode, keys string) {
	t.Helper()
	t.Cleanup(func() { _ = ConfigureSignatureVerification(VerifyModeOff, "", "", "") })
	if err := ConfigureSignatureVerification(mode, "crosslink-license", "crosslink", keys); err != nil {
		t.Fatalf("configure %s: %v", mode, err)
	}
}

func stdClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"lid": 1, "sub": "cust-1", "tier": "enterprise",
		"exp": time.Now().Add(24 * time.Hour).Unix(),
		"iss": "crosslink-license", "aud": "crosslink", "fp": "aa:bb",
	}
}

// Valid token verifies; tampering the payload (forged) fails.
func TestVerify_ValidAndForged(t *testing.T) {
	k := newTestKey(t)
	setMode(t, VerifyModeEnforce, keyJSON(k))
	good := k.sign(t, "k1", stdClaims())
	if err := VerifyRawToken(good); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	// forge: flip a claim on the signed body
	parts := strings.Split(good, ".")
	forgedClaims := stdClaims()
	forgedClaims["tier"] = "enterprise-forged"
	hdr, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(forgedClaims)
	forged := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body) + "." + parts[2]
	if err := VerifyRawToken(forged); err == nil {
		t.Fatal("forged payload accepted")
	}
}

// Wrong key in the whitelist (attacker-signed, unknown kid or known kid with
// different key) must fail.
func TestVerify_WrongKey(t *testing.T) {
	good := newTestKey(t)
	attacker := newTestKey(t)
	setMode(t, VerifyModeEnforce, keyJSON(good))
	// unknown kid
	if err := VerifyRawToken(attacker.sign(t, "k-evil", stdClaims())); err == nil {
		t.Fatal("unknown-kid token accepted")
	}
	// whitelisted kid but signed by another key
	if err := VerifyRawToken(attacker.sign(t, "k1", stdClaims())); err == nil {
		t.Fatal("wrong-key token accepted under whitelisted kid")
	}
}

// Expired tokens fail (exp required and validated).
func TestVerify_Expired(t *testing.T) {
	k := newTestKey(t)
	setMode(t, VerifyModeEnforce, keyJSON(k))
	claims := stdClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	if err := VerifyRawToken(k.sign(t, "k1", claims)); err == nil {
		t.Fatal("expired token accepted")
	}
	noExp := stdClaims()
	delete(noExp, "exp")
	if err := VerifyRawToken(k.sign(t, "k1", noExp)); err == nil {
		t.Fatal("missing-exp token accepted")
	}
}

// Algorithm confusion: HS256 (symmetric) with the Ed25519 public bytes as
// HMAC secret, and alg=none — both rejected before key lookup.
func TestVerify_AlgConfusion(t *testing.T) {
	k := newTestKey(t)
	setMode(t, VerifyModeEnforce, keyJSON(k))
	// HS256 signed with the public key bytes as the HMAC secret
	hmacTok := jwt.NewWithClaims(jwt.SigningMethodHS256, stdClaims())
	hmacTok.Header["kid"] = "k1"
	hs, err := hmacTok.SignedString([]byte(base64.StdEncoding.EncodeToString(k.pub)))
	if err != nil {
		t.Fatalf("hs sign: %v", err)
	}
	if err := VerifyRawToken(hs); err == nil {
		t.Fatal("HS256 confusion token accepted")
	}
	// alg=none
	hdr, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(stdClaims())
	noneTok := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body) + "."
	if err := VerifyRawToken(noneTok); err == nil {
		t.Fatal("alg=none token accepted")
	}
}

// iss/aud mismatch fails when configured; rotation: two kids both verify;
// removing a kid revokes it.
func TestVerify_IssuerAudienceAndRotation(t *testing.T) {
	k1 := newTestKey(t)
	k2 := newTestKey(t)
	twoKeys := `{"k1":"` + base64.StdEncoding.EncodeToString(k1.pub) + `","k2":"` + base64.StdEncoding.EncodeToString(k2.pub) + `"}`
	setMode(t, VerifyModeEnforce, twoKeys)

	if err := VerifyRawToken(k1.sign(t, "k1", stdClaims())); err != nil {
		t.Fatalf("kid k1 rejected: %v", err)
	}
	if err := VerifyRawToken(k2.sign(t, "k2", stdClaims())); err != nil {
		t.Fatalf("kid k2 (rotation) rejected: %v", err)
	}
	// revoke k1 by shipping only k2
	setMode(t, VerifyModeEnforce, `{"k2":"`+base64.StdEncoding.EncodeToString(k2.pub)+`"}`)
	if err := VerifyRawToken(k1.sign(t, "k1", stdClaims())); err == nil {
		t.Fatal("revoked kid still accepted")
	}
	if err := VerifyRawToken(k2.sign(t, "k2", stdClaims())); err != nil {
		t.Fatalf("surviving kid rejected: %v", err)
	}

	// iss/aud mismatch
	setMode(t, VerifyModeEnforce, twoKeys)
	badIss := stdClaims()
	badIss["iss"] = "evil"
	if err := VerifyRawToken(k1.sign(t, "k1", badIss)); err == nil {
		t.Fatal("wrong issuer accepted")
	}
	badAud := stdClaims()
	badAud["aud"] = "other-product"
	if err := VerifyRawToken(k1.sign(t, "k1", badAud)); err == nil {
		t.Fatal("wrong audience accepted")
	}
}

// Mode matrix: off accepts everything (no keys needed); warn verifies and
// still ACCEPTS failures (observable but non-breaking — the migration
// window); enforce rejects. Misconfiguration (enforce without keys) is a
// configure error and falls back safe.
func TestVerify_ModeMatrixAndMisconfig(t *testing.T) {
	k := newTestKey(t)
	attacker := newTestKey(t)

	// off: no checks even for garbage
	if err := ConfigureSignatureVerification(VerifyModeOff, "", "", ""); err != nil {
		t.Fatalf("off configure: %v", err)
	}
	if err := VerifyRawToken("not-a-jwt"); err != nil {
		t.Fatalf("off mode must skip checks, got %v", err)
	}
	// warn: verifies (bad token WOULD fail here) but the mode semantic is
	// applied by the caller (log+accept); VerifyRawToken itself reports.
	if err := ConfigureSignatureVerification(VerifyModeWarn, "", "", keyJSON(k)); err != nil {
		t.Fatalf("warn configure: %v", err)
	}
	if err := VerifyRawToken(attacker.sign(t, "k1", stdClaims())); err == nil {
		t.Fatal("warn mode must still DETECT failures (caller decides)")
	}
	// enforce without keys = configuration error
	if err := ConfigureSignatureVerification(VerifyModeEnforce, "", "", ""); err == nil {
		t.Fatal("enforce without keys must be a configure error")
	}
	// unknown mode rejected
	if err := ConfigureSignatureVerification("sometimes", "", "", keyJSON(k)); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
}

// PEM whitelist form (with kid comments) parses and verifies — the shape an
// operator would ship on disk behind "@path".
func TestVerify_PEMWhitelistForm(t *testing.T) {
	k := newTestKey(t)
	pemPub := base64.StdEncoding.EncodeToString([]byte{})
	_ = pemPub
	pemText := `# kid=k1
-----BEGIN PUBLIC KEY-----
` + base64.StdEncoding.EncodeToString(rawPKIX(t, k)) + `
-----END PUBLIC KEY-----
`
	setMode(t, VerifyModeEnforce, pemText)
	if err := VerifyRawToken(k.sign(t, "k1", stdClaims())); err != nil {
		t.Fatalf("PEM-whitelisted token rejected: %v", err)
	}
	if err := VerifyRawToken(newTestKey(t).sign(t, "k1", stdClaims())); err == nil {
		t.Fatal("wrong key accepted via PEM whitelist")
	}
}

func rawPKIX(t *testing.T, k testKeys) []byte {
	t.Helper()
	// raw 32-byte form is also accepted by the loader; use it for the PEM body
	return []byte(k.pub)
}
