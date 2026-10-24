package license

import (
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	gmSm2 "github.com/tjfoc/gmsm/sm2"
	smx509 "github.com/tjfoc/gmsm/x509"
)

// U-37 multi-algorithm verification tests, mirroring the ISSUER's exact
// signing semantics (License-Server, audited read-only):
//   RS256  — RSA-2048, SHA-256, PKCS#1 v1.5 (internal/crypto/standard.go)
//   SM2SM3 — gm provider priv.Sign(rand, signingString, nil) /
//            pub.Verify(signingString, sig) (internal/crypto/gm_jwt.go)
//   EdDSA  — OPTIONAL future path only; kept exercised for the two-sided
//            migration plan (decision-packages §B), NOT enabled issuer-side.
// ALL keys here are EPHEMERAL TEST KEYS generated in-process — this suite
// never ships, writes, or discloses any production key material (production
// signing keys are held by the user).

// tokenSigner is one algorithm family's mint capability.
type tokenSigner interface {
	alg() string
	material() string          // whitelist entry (PEM or b64 raw)
	sign(t *testing.T, ss string) string // signature, base64url
}

// ---- RS256 (the standard issuer path) ----

type rs256Key struct{ priv *rsa.PrivateKey }

func newRS256(t *testing.T) rs256Key {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	return rs256Key{priv: priv}
}

func (k rs256Key) alg() string { return "RS256" }

func (k rs256Key) material() string {
	der, _ := x509.MarshalPKIXPublicKey(&k.priv.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func (k rs256Key) pkcs1Material() string {
	der := x509.MarshalPKCS1PublicKey(&k.priv.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: der}))
}

func (k rs256Key) sign(t *testing.T, ss string) string {
	t.Helper()
	h := sha256.Sum256([]byte(ss))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.priv, crypto.SHA256, h[:])
	if err != nil {
		t.Fatalf("rs256 sign: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(sig)
}

// ---- SM2SM3 (the GM issuer path) ----

type sm2Key struct{ priv *gmSm2.PrivateKey }

func newSM2(t *testing.T) sm2Key {
	t.Helper()
	priv, err := gmSm2.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen sm2: %v", err)
	}
	return sm2Key{priv: priv}
}

func (k sm2Key) alg() string { return "SM2SM3" }

// material mirrors the server's export (gm.go: MarshalSm2PublicKey under a
// "SM2 PUBLIC KEY" block) — the form the admin signing-key endpoint serves.
func (k sm2Key) material() string {
	der, err := smx509.MarshalSm2PublicKey(&k.priv.PublicKey)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "SM2 PUBLIC KEY", Bytes: der}))
}

// genericMaterial uses tjfoc's own writer shape ("PUBLIC KEY").
func (k sm2Key) genericMaterial() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.der()}))
}

func (k sm2Key) der() []byte {
	der, err := smx509.MarshalSm2PublicKey(&k.priv.PublicKey)
	if err != nil {
		panic(err)
	}
	return der
}

func (k sm2Key) sign(t *testing.T, ss string) string {
	t.Helper()
	sig, err := k.priv.Sign(rand.Reader, []byte(ss), nil) // server's exact call
	if err != nil {
		t.Fatalf("sm2 sign: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(sig)
}

// ---- EdDSA (future path) ----

type edKey struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newEd(t *testing.T) edKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ed: %v", err)
	}
	return edKey{priv: priv, pub: pub}
}

func (k edKey) alg() string { return "EdDSA" }

func (k edKey) material() string { return base64.StdEncoding.EncodeToString(k.pub) }

func (k edKey) sign(t *testing.T, ss string) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.priv, []byte(ss)))
}

// ---- shared mint/config helpers (manual JWT assembly, like the server) ----

func mint(t *testing.T, k tokenSigner, kid string, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": k.alg(), "typ": "JWT", "kid": kid})
	body, _ := json.Marshal(claims)
	ss := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	return ss + "." + k.sign(t, ss)
}

func whitelist(kids ...string) string {
	return "{" + strings.Join(kids, ",") + "}"
}

func kidJSON(kid, material string) string {
	m, _ := json.Marshal(material)
	return `"` + kid + `":` + string(m)
}

func setMode(t *testing.T, mode string, entries ...string) {
	t.Helper()
	setModeSpec(t, mode, "{"+strings.Join(entries, ",")+"}")
}

func setModeSpec(t *testing.T, mode, spec string) {
	t.Helper()
	t.Cleanup(func() { _ = ConfigureSignatureVerification(VerifyModeOff, "", "", "") })
	if err := ConfigureSignatureVerification(mode, "crosslink-license-server", "crosslink", spec); err != nil {
		t.Fatalf("configure %s: %v", mode, err)
	}
}

// stdClaims mirrors the server's claim shape (license_service.go): iss, sub,
// lid, tier, max_nodes, grace_days, fp, type, iat, exp.
func stdClaims() map[string]any {
	return map[string]any{
		"iss": "crosslink-license-server", "sub": "cust-1", "lid": 1,
		"tier": "enterprise", "max_nodes": 10, "grace_days": 7,
		"fp": "aa:bb", "type": "node", "aud": "crosslink",
		"iat": time.Now().Unix(), "exp": time.Now().Add(24 * time.Hour).Unix(),
	}
}

// RS256 — the standard issuer path end-to-end: valid token (server claim
// shape) verifies under a whitelisted kid; tampering the payload fails.
func TestVerify_RS256ValidAndForged(t *testing.T) {
	k := newRS256(t)
	setMode(t, VerifyModeEnforce, kidJSON("k1", k.material()))
	good := mint(t, k, "k1", stdClaims())
	if err := VerifyRawToken(good); err != nil {
		t.Fatalf("valid RS256 token rejected: %v", err)
	}
	parts := strings.Split(good, ".")
	forged := stdClaims()
	forged["tier"] = "enterprise-forged"
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(forged)
	bad := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body) + "." + parts[2]
	if err := VerifyRawToken(bad); err == nil {
		t.Fatal("forged RS256 payload accepted")
	}
}

// RS256 — attacker keys fail under both an unknown kid and a whitelisted kid.
func TestVerify_RS256WrongKeyAndUnknownKid(t *testing.T) {
	good := newRS256(t)
	attacker := newRS256(t)
	setMode(t, VerifyModeEnforce, kidJSON("k1", good.material()))
	if err := VerifyRawToken(mint(t, attacker, "k-evil", stdClaims())); err == nil {
		t.Fatal("unknown-kid RS256 token accepted")
	}
	if err := VerifyRawToken(mint(t, attacker, "k1", stdClaims())); err == nil {
		t.Fatal("wrong-key RS256 token accepted under whitelisted kid")
	}
}

// RS256 — exp required and enforced.
func TestVerify_RS256Expiry(t *testing.T) {
	k := newRS256(t)
	setMode(t, VerifyModeEnforce, kidJSON("k1", k.material()))
	expired := stdClaims()
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	if err := VerifyRawToken(mint(t, k, "k1", expired)); err == nil {
		t.Fatal("expired RS256 token accepted")
	}
	noExp := stdClaims()
	delete(noExp, "exp")
	if err := VerifyRawToken(mint(t, k, "k1", noExp)); err == nil {
		t.Fatal("missing-exp RS256 token accepted")
	}
}

// Algorithm confusion is rejected before any key use: HS256 with the public
// key material as HMAC secret, alg=none, and an alg/kid family mismatch
// (RS256 token under an SM2 kid and vice versa).
func TestVerify_AlgConfusion(t *testing.T) {
	rs := newRS256(t)
	sm := newSM2(t)
	ed := newEd(t)
	setMode(t, VerifyModeEnforce,
		kidJSON("k-rs", rs.material()), kidJSON("k-sm", sm.material()), kidJSON("k-ed", ed.material()))

	// HS256 "signed" with the public key PEM bytes as the HMAC secret
	hmacSecret := rs.material()
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT", "kid": "k-rs"})
	body, _ := json.Marshal(stdClaims())
	ss := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	sig := base64.RawURLEncoding.EncodeToString(hmacSHA256(hmacSecret, ss))
	if err := VerifyRawToken(ss + "." + sig); err == nil {
		t.Fatal("HS256 confusion token accepted")
	}
	// alg=none
	hdrNone, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT", "kid": "k-rs"})
	noneTok := base64.RawURLEncoding.EncodeToString(hdrNone) + "." + base64.RawURLEncoding.EncodeToString(body) + "."
	if err := VerifyRawToken(noneTok); err == nil {
		t.Fatal("alg=none token accepted")
	}
	// RS256 token presented under the SM2 kid (family mismatch)
	if err := VerifyRawToken(mint(t, rs, "k-sm", stdClaims())); err == nil {
		t.Fatal("RS256 token accepted under SM2 kid")
	}
	// SM2 token presented under the RSA kid
	if err := VerifyRawToken(mint(t, sm, "k-rs", stdClaims())); err == nil {
		t.Fatal("SM2 token accepted under RSA kid")
	}
	// EdDSA token presented under the RS256 kid
	if err := VerifyRawToken(mint(t, ed, "k-rs", stdClaims())); err == nil {
		t.Fatal("EdDSA token accepted under RSA kid")
	}
}

// SM2SM3 — the GM issuer path: server-shaped "SM2 PUBLIC KEY" PEM whitelists,
// valid token verifies (server's exact Sign/Verify calls), forgery fails, and
// tjfoc's generic "PUBLIC KEY" PEM form of the same key also loads.
func TestVerify_SM2ValidAndForged(t *testing.T) {
	k := newSM2(t)
	attacker := newSM2(t)
	setMode(t, VerifyModeEnforce, kidJSON("k1", k.material()))
	if err := VerifyRawToken(mint(t, k, "k1", stdClaims())); err != nil {
		t.Fatalf("valid SM2SM3 token rejected: %v", err)
	}
	// forged payload
	good := mint(t, k, "k1", stdClaims())
	parts := strings.Split(good, ".")
	forged := stdClaims()
	forged["tier"] = "enterprise-forged"
	hdr, _ := json.Marshal(map[string]string{"alg": "SM2SM3", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(forged)
	bad := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body) + "." + parts[2]
	if err := VerifyRawToken(bad); err == nil {
		t.Fatal("forged SM2 payload accepted")
	}
	// attacker SM2 key under whitelisted kid
	if err := VerifyRawToken(mint(t, attacker, "k1", stdClaims())); err == nil {
		t.Fatal("wrong-key SM2 token accepted")
	}
	// generic "PUBLIC KEY" PEM form of the same key
	setMode(t, VerifyModeEnforce, kidJSON("k1", k.genericMaterial()))
	if err := VerifyRawToken(mint(t, k, "k1", stdClaims())); err != nil {
		t.Fatalf("SM2 token rejected via generic PUBLIC KEY PEM: %v", err)
	}
}

// Rotation mirrors RotateKey: a new kid (possibly a different algorithm
// family) verifies alongside the old one; removing the old kid revokes it.
// Mixed-family whitelist also proves the alg/kid binding is per-key.
func TestVerify_RotationAndRevocation(t *testing.T) {
	rs1 := newRS256(t)
	rs2 := newRS256(t)
	sm := newSM2(t)
	setMode(t, VerifyModeEnforce,
		kidJSON("k1", rs1.material()), kidJSON("k2", rs2.material()), kidJSON("k3", sm.material()))
	if err := VerifyRawToken(mint(t, rs1, "k1", stdClaims())); err != nil {
		t.Fatalf("kid k1 rejected: %v", err)
	}
	if err := VerifyRawToken(mint(t, rs2, "k2", stdClaims())); err != nil {
		t.Fatalf("kid k2 (rotated-in) rejected: %v", err)
	}
	if err := VerifyRawToken(mint(t, sm, "k3", stdClaims())); err != nil {
		t.Fatalf("kid k3 (rotated-in, GM family) rejected: %v", err)
	}
	// revoke k1: only k2+k3 shipped
	setMode(t, VerifyModeEnforce, kidJSON("k2", rs2.material()), kidJSON("k3", sm.material()))
	if err := VerifyRawToken(mint(t, rs1, "k1", stdClaims())); err == nil {
		t.Fatal("revoked kid still accepted")
	}
	if err := VerifyRawToken(mint(t, rs2, "k2", stdClaims())); err != nil {
		t.Fatalf("surviving kid rejected: %v", err)
	}
}

// iss/aud are checked when configured.
func TestVerify_IssuerAudience(t *testing.T) {
	k := newRS256(t)
	setMode(t, VerifyModeEnforce, kidJSON("k1", k.material()))
	badIss := stdClaims()
	badIss["iss"] = "evil"
	if err := VerifyRawToken(mint(t, k, "k1", badIss)); err == nil {
		t.Fatal("wrong issuer accepted")
	}
	badAud := stdClaims()
	badAud["aud"] = "other-product"
	if err := VerifyRawToken(mint(t, k, "k1", badAud)); err == nil {
		t.Fatal("wrong audience accepted")
	}
}

// EdDSA remains loadable and correct as the OPTIONAL future path (two-sided
// migration plan) — but only under its own kid and family.
func TestVerify_EdDSAFuturePath(t *testing.T) {
	k := newEd(t)
	attacker := newEd(t)
	setMode(t, VerifyModeEnforce, kidJSON("k1", k.material()))
	if err := VerifyRawToken(mint(t, k, "k1", stdClaims())); err != nil {
		t.Fatalf("valid EdDSA token rejected: %v", err)
	}
	if err := VerifyRawToken(mint(t, attacker, "k1", stdClaims())); err == nil {
		t.Fatal("wrong-key EdDSA token accepted")
	}
	expired := stdClaims()
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	if err := VerifyRawToken(mint(t, k, "k1", expired)); err == nil {
		t.Fatal("expired EdDSA token accepted")
	}
}

// Mode matrix: off accepts everything; warn still DETECTS (caller logs and
// accepts); enforce without keys is a configure error; unknown modes rejected.
func TestVerify_ModeMatrixAndMisconfig(t *testing.T) {
	k := newRS256(t)
	attacker := newRS256(t)

	if err := ConfigureSignatureVerification(VerifyModeOff, "", "", ""); err != nil {
		t.Fatalf("off configure: %v", err)
	}
	if err := VerifyRawToken("not-a-jwt"); err != nil {
		t.Fatalf("off mode must skip checks, got %v", err)
	}
	if err := ConfigureSignatureVerification(VerifyModeWarn, "", "", "{"+kidJSON("k1", k.material())+"}"); err != nil {
		t.Fatalf("warn configure: %v", err)
	}
	if err := VerifyRawToken(mint(t, attacker, "k1", stdClaims())); err == nil {
		t.Fatal("warn mode must still DETECT failures (caller decides)")
	}
	if err := ConfigureSignatureVerification(VerifyModeEnforce, "", "", ""); err == nil {
		t.Fatal("enforce without keys must be a configure error")
	}
	if err := ConfigureSignatureVerification("sometimes", "", "", "{"+kidJSON("k1", k.material())+"}"); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
}

// PEM whitelist forms an operator would ship on disk behind "@path": a PEM
// list with "# kid=" comments covering PKIX, PKCS#1 and SM2 blocks.
func TestVerify_PEMWhitelistForm(t *testing.T) {
	rs := newRS256(t)
	rs2 := newRS256(t)
	sm := newSM2(t)
	pemList := "# kid=a\n" + strings.TrimSpace(rs.material()) + "\n# kid=b\n" +
		strings.TrimSpace(rs2.pkcs1Material()) + "\n# kid=c\n" + strings.TrimSpace(sm.material()) + "\n"
	setModeSpec(t, VerifyModeEnforce, pemList)
	if err := VerifyRawToken(mint(t, rs, "a", stdClaims())); err != nil {
		t.Fatalf("PEM-whitelisted PKIX RSA rejected: %v", err)
	}
	if err := VerifyRawToken(mint(t, rs2, "b", stdClaims())); err != nil {
		t.Fatalf("PEM-whitelisted PKCS#1 RSA rejected: %v", err)
	}
	if err := VerifyRawToken(mint(t, sm, "c", stdClaims())); err != nil {
		t.Fatalf("PEM-whitelisted SM2 rejected: %v", err)
	}
	if err := VerifyRawToken(mint(t, newRS256(t), "a", stdClaims())); err == nil {
		t.Fatal("wrong key accepted via PEM whitelist")
	}
}

func hmacSHA256(secret, msg string) []byte {
	// test-only alg-confusion token (HS256), stdlib HMAC
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// ---------------------------------------------------------------------------
// Behavior matrix (security review 2026-10-24): every configured mode ×
// keys present/absent, as the executable twin of the report matrix.
//
//	configured | keys | effective | licensed-token decision
//	-----------+------+-----------+------------------------------------------
//	auto       | yes  | enforce   | verify; signature failures rejected
//	auto       | no   | enforce*  | EVERY licensed token rejected (fail
//	                        closed, note set; Community unaffected)
//	off        | any  | off       | no checks — development only, explicit
//	warn       | yes  | warn      | verify; failures reported, caller accepts
//	warn       | no   | cfg error | wired via config -> lockdown
//	enforce    | yes  | enforce   | verify; signature failures rejected
//	enforce    | no   | cfg error | wired via config -> lockdown
//	any        | bad  | cfg error | wired via config -> lockdown
//
// * auto-keyless never relaxes to "accept unverified" — un-migrated
// deployments follow the upgrade checklist instead.
func TestVerify_AutoOffWarnEnforceMatrix(t *testing.T) {
	good := newRS256(t)
	bad := newRS256(t)
	whitelistSpec := whitelist(kidJSON("k1", good.material()))
	goodToken := mint(t, good, "k1", stdClaims())
	forgedToken := mint(t, bad, "k1", stdClaims())
	t.Cleanup(func() { _ = ConfigureSignatureVerification(VerifyModeOff, "", "", "") })

	// auto + keys -> enforce semantics.
	if err := ConfigureSignatureVerification(VerifyModeAuto, "", "", whitelistSpec); err != nil {
		t.Fatalf("auto+keys configure: %v", err)
	}
	if VerificationMode() != VerifyModeEnforce || VerificationConfiguredMode() != VerifyModeAuto {
		t.Fatalf("auto+keys must run enforce (configured auto), got mode=%q configured=%q",
			VerificationMode(), VerificationConfiguredMode())
	}
	if VerificationNote() != "" {
		t.Fatal("auto+keys must not carry a note")
	}
	if err := VerifyRawToken(goodToken); err != nil {
		t.Fatalf("auto+keys rejected a valid token: %v", err)
	}
	if err := VerifyRawToken(forgedToken); err == nil {
		t.Fatal("auto+keys accepted a forged token")
	}

	// auto keyless -> fail-closed enforce: EVERY licensed token rejected,
	// reason recorded, configured mode still reported as auto.
	if err := ConfigureSignatureVerification(VerifyModeAuto, "", "", ""); err != nil {
		t.Fatalf("auto keyless configure: %v", err)
	}
	if VerificationMode() != VerifyModeEnforce {
		t.Fatalf("auto keyless must stay enforce, got %q", VerificationMode())
	}
	if VerificationNote() == "" {
		t.Fatal("auto keyless must record the fail-closed reason")
	}
	if err := VerifyRawToken(goodToken); err == nil {
		t.Fatal("auto keyless accepted a licensed token (must reject ALL)")
	}

	// empty configured mode resolves to auto (the shipped default).
	if err := ConfigureSignatureVerification("", "", "", whitelistSpec); err != nil {
		t.Fatalf("empty mode configure: %v", err)
	}
	if VerificationConfiguredMode() != VerifyModeAuto || VerificationMode() != VerifyModeEnforce {
		t.Fatalf("empty mode must resolve to auto/enforce, got %q/%q",
			VerificationConfiguredMode(), VerificationMode())
	}

	// explicit off -> no checks (development only).
	if err := ConfigureSignatureVerification(VerifyModeOff, "", "", ""); err != nil {
		t.Fatalf("off configure: %v", err)
	}
	if VerificationMode() != VerifyModeOff || VerificationNote() != "" {
		t.Fatalf("explicit off must be off without notes, got %q/%q", VerificationMode(), VerificationNote())
	}
	if err := VerifyRawToken(forgedToken); err != nil {
		t.Fatalf("explicit off skips checks, got %v", err)
	}

	// warn + keys -> detects failures (caller decides), valid passes.
	if err := ConfigureSignatureVerification(VerifyModeWarn, "", "", whitelistSpec); err != nil {
		t.Fatalf("warn+keys configure: %v", err)
	}
	if VerificationMode() != VerifyModeWarn {
		t.Fatalf("warn must stay warn, got %q", VerificationMode())
	}
	if err := VerifyRawToken(goodToken); err != nil {
		t.Fatalf("warn rejected valid token: %v", err)
	}
	if err := VerifyRawToken(forgedToken); err == nil {
		t.Fatal("warn must still DETECT failures")
	}

	// enforce + keys -> enforce semantics.
	if err := ConfigureSignatureVerification(VerifyModeEnforce, "", "", whitelistSpec); err != nil {
		t.Fatalf("enforce+keys configure: %v", err)
	}
	if err := VerifyRawToken(goodToken); err != nil {
		t.Fatalf("enforce rejected valid token: %v", err)
	}
	if err := VerifyRawToken(forgedToken); err == nil {
		t.Fatal("enforce accepted a forged token")
	}

	// keyless warn/enforce are configuration errors (never unverified).
	if err := ConfigureSignatureVerification(VerifyModeWarn, "", "", ""); err == nil {
		t.Fatal("keyless warn must be a configure error")
	}
	if err := ConfigureSignatureVerification(VerifyModeEnforce, "", "", ""); err == nil {
		t.Fatal("keyless enforce must be a configure error")
	}
}

// configureVerificationFromConfig turns every configuration error into
// lockdown (reject-everything), never a silent unverified fallback; a valid
// config afterwards clears the state.
func TestVerify_ConfigMisconfigurationLocksDown(t *testing.T) {
	t.Cleanup(func() {
		_ = ConfigureSignatureVerification(VerifyModeOff, "", "", "")
	})
	cases := map[string]struct{ mode, keys string }{
		"keyless-enforce": {VerifyModeEnforce, ""},
		"keyless-warn":    {VerifyModeWarn, ""},
		"auto-bad-keys":   {VerifyModeAuto, "@/no/such/file.json"},
		"unknown-mode":    {"sometimes", ""},
	}
	for name, c := range cases {
		configureVerificationFromConfig(config.LicenseConfig{VerifyMode: c.mode, VerifyKeys: c.keys})
		if VerificationNote() == "" {
			t.Fatalf("%s: must lock down with a note", name)
		}
		if VerificationMode() != VerifyModeEnforce {
			t.Fatalf("%s: lockdown must run as enforce, got %q", name, VerificationMode())
		}
		if err := VerifyRawToken("anything"); err == nil {
			t.Fatalf("%s: lockdown must reject every token", name)
		}
	}

	// A valid configuration clears the lockdown (recoverable, not sticky).
	configureVerificationFromConfig(config.LicenseConfig{VerifyMode: VerifyModeOff})
	if VerificationNote() != "" {
		t.Fatal("valid config must clear the lockdown note")
	}
}
