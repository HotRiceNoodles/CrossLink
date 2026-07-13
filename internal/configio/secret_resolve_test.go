package configio

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/secret"
)

func testEncStore(t *testing.T) *secret.EncryptedDBStore {
	t.Helper()
	cp, _ := crypto.NewProvider("standard")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, 32)) // AES-256 32-byte key
	store, err := secret.NewEncryptedDBStore(key, cp)
	if err != nil {
		t.Fatalf("NewEncryptedDBStore: %v", err)
	}
	return store
}

func TestResolveForExport_Plaintext(t *testing.T) {
	got, err := resolveForExport(testEncStore(t), "sk-plain")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "sk-plain" {
		t.Errorf("plaintext should pass through, got %q", got)
	}
}

func TestResolveForExport_EnvReferencePreserved(t *testing.T) {
	got, err := resolveForExport(testEncStore(t), "env://MY_API_KEY")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "env://MY_API_KEY" {
		t.Errorf("env:// reference must be preserved verbatim, got %q", got)
	}
}

func TestResolveForExport_EncReferenceDecrypted(t *testing.T) {
	store := testEncStore(t)
	enc, err := store.Encrypt("sk-secret-value")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(enc, "enc") {
		t.Fatalf("expected enc prefix, got %q", enc)
	}
	got, err := resolveForExport(store, enc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "sk-secret-value" {
		t.Errorf("enc:// should decrypt to plaintext, got %q", got)
	}
}

func TestResolveForExport_EncButNoEncStore(t *testing.T) {
	_, err := resolveForExport(nil, "enc2://c29tZXRoaW5n")
	if err == nil {
		t.Fatal("expected error when enc:// present but encStore is nil")
	}
}

func TestResolveForImport_PlaintextReencrypted(t *testing.T) {
	store := testEncStore(t)
	got, err := resolveForImport(store, "sk-plain")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got == "sk-plain" {
		t.Fatal("plaintext should be re-encrypted, not stored verbatim")
	}
	// Round-trip: the re-encrypted value should decrypt back to the original.
	dec, err := store.Decrypt(got)
	if err != nil {
		t.Fatalf("Decrypt round-trip: %v", err)
	}
	if dec != "sk-plain" {
		t.Errorf("round-trip mismatch: got %q", dec)
	}
}

func TestResolveForImport_NilEncStoreStoresPlaintext(t *testing.T) {
	got, err := resolveForImport(nil, "sk-plain")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "sk-plain" {
		t.Errorf("nil encStore should store plaintext, got %q", got)
	}
}

func TestResolveForImport_EnvReferencePreserved(t *testing.T) {
	got, err := resolveForImport(testEncStore(t), "env://MY_API_KEY")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "env://MY_API_KEY" {
		t.Errorf("env:// must pass through import verbatim, got %q", got)
	}
}

func TestResolveExtraConfigExportImportRoundTrip(t *testing.T) {
	store := testEncStore(t)
	// extra_config with one sensitive (encrypted) and one non-sensitive field.
	encVal, _ := store.Encrypt("AKIASECRET")
	raw := []byte(`{"access_key_id":"` + encVal + `","api_protocol":"openai","env_ref":"env://AWS_KEY"}`)
	// Note: env_ref is not in IsSensitiveField list, so it's preserved as env:// anyway.

	exported, err := resolveExtraConfigForExport(store, raw)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// access_key_id must be decrypted to plaintext in the export.
	if got := exported["access_key_id"]; got != "AKIASECRET" {
		t.Errorf("access_key_id should decrypt to plaintext, got %v", got)
	}
	if got := exported["api_protocol"]; got != "openai" {
		t.Errorf("non-sensitive field should pass through, got %v", got)
	}

	// Import side: re-encrypt under a fresh store.
	store2 := testEncStore(t)
	imported, err := resolveExtraConfigForImport(store2, exported)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// The imported access_key_id must NOT be plaintext (re-encrypted), and must
	// round-trip back to AKIASECRET under the target store.
	s := string(imported)
	if strings.Contains(s, "AKIASECRET") {
		t.Errorf("imported extra_config must not contain plaintext secret: %s", s)
	}
	var m map[string]any
	if err := json.Unmarshal(imported, &m); err != nil {
		t.Fatalf("unmarshal imported: %v", err)
	}
	dec, err := store2.Decrypt(m["access_key_id"].(string))
	if err != nil {
		t.Fatalf("Decrypt imported access_key_id: %v", err)
	}
	if dec != "AKIASECRET" {
		t.Errorf("imported access_key_id round-trip mismatch: %q", dec)
	}
}
