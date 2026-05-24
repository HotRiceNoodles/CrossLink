package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/crypto"
)

func testMasterKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func testCryptoProvider() crypto.CryptoProvider {
	p, _ := crypto.NewProvider("standard")
	return p
}

func TestEncryptedRoundTrip(t *testing.T) {
	enc, err := NewEncryptedDBStore(testMasterKey(), testCryptoProvider())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	plaintext := "sk-abc123-secret-key"
	encrypted, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !enc.IsEncrypted(encrypted) {
		t.Error("IsEncrypted should return true for encrypted value")
	}

	decrypted, err := enc.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if decrypted != plaintext {
		t.Errorf("got %q, want %q", decrypted, plaintext)
	}
}

func TestEncryptedGetSecret(t *testing.T) {
	enc, err := NewEncryptedDBStore(testMasterKey(), testCryptoProvider())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	// Test through resolver (matches real usage: scheme is stripped before GetSecret)
	r := NewSecretResolver(5 * time.Minute)
	r.Register(enc)
	r.Register(enc.AsV2())

	encrypted, _ := enc.Encrypt("test-value")
	val, err := r.Resolve(context.Background(), encrypted)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if val != "test-value" {
		t.Errorf("got %q, want %q", val, "test-value")
	}
}

func TestEncryptedV2StoreName(t *testing.T) {
	enc, _ := NewEncryptedDBStore(testMasterKey(), testCryptoProvider())
	v2 := enc.AsV2()
	if v2.Name() != "enc2" {
		t.Errorf("AsV2().Name() = %q, want %q", v2.Name(), "enc2")
	}
}

func TestEncryptedInvalidKey(t *testing.T) {
	_, err := NewEncryptedDBStore("not-valid-base64!!!", testCryptoProvider())
	if err == nil {
		t.Error("expected error for invalid base64")
	}

	shortKey := base64.StdEncoding.EncodeToString([]byte("short"))
	_, err = NewEncryptedDBStore(shortKey, testCryptoProvider())
	if err == nil {
		t.Error("expected error for key not 32 bytes")
	}
}

func TestEncryptedTamperedCiphertext(t *testing.T) {
	enc, _ := NewEncryptedDBStore(testMasterKey(), testCryptoProvider())
	encrypted, _ := enc.Encrypt("secret")

	// Tamper with the encrypted value
	prefix := "enc2://"
	raw := strings.TrimPrefix(encrypted, prefix)
	data, _ := base64.StdEncoding.DecodeString(raw)
	if len(data) > 0 {
		data[0] ^= 0xFF
	}
	tampered := prefix + base64.StdEncoding.EncodeToString(data)

	_, err := enc.Decrypt(tampered)
	if err == nil {
		t.Error("expected error for tampered ciphertext")
	}
}

func TestEncryptedEmptyString(t *testing.T) {
	enc, _ := NewEncryptedDBStore(testMasterKey(), testCryptoProvider())
	encrypted, err := enc.Encrypt("")
	if err != nil {
		t.Fatalf("encrypt empty: %v", err)
	}
	decrypted, err := enc.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt empty: %v", err)
	}
	if decrypted != "" {
		t.Errorf("got %q, want empty", decrypted)
	}
}

func TestEncryptedStoreName(t *testing.T) {
	enc, _ := NewEncryptedDBStore(testMasterKey(), testCryptoProvider())
	if enc.Name() != "enc" {
		t.Errorf("Name() = %q, want %q", enc.Name(), "enc")
	}
}

func TestEncryptedDifferentNonces(t *testing.T) {
	enc, _ := NewEncryptedDBStore(testMasterKey(), testCryptoProvider())
	enc1, _ := enc.Encrypt("same-input")
	enc2, _ := enc.Encrypt("same-input")
	if enc1 == enc2 {
		t.Error("encrypting same value twice should produce different ciphertexts (random nonce)")
	}
}

func TestEncryptedV2Prefix(t *testing.T) {
	enc, _ := NewEncryptedDBStore(testMasterKey(), testCryptoProvider())
	encrypted, _ := enc.Encrypt("test")
	if !strings.HasPrefix(encrypted, "enc2://") {
		t.Errorf("Encrypt should produce enc2:// prefix, got %q", encrypted[:10])
	}
}

func TestEncryptedGMMode(t *testing.T) {
	gmKey := base64.StdEncoding.EncodeToString(make([]byte, 16)) // SM4: 16 bytes
	gmProvider, _ := crypto.NewProvider("gm")

	enc, err := NewEncryptedDBStore(gmKey, gmProvider)
	if err != nil {
		t.Fatalf("create GM store: %v", err)
	}

	plaintext := "sk-gm-secret-key"
	encrypted, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt (GM): %v", err)
	}
	if !strings.HasPrefix(encrypted, "enc2://") {
		t.Errorf("GM encrypt should produce enc2:// prefix, got %q", encrypted[:10])
	}

	decrypted, err := enc.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt (GM): %v", err)
	}
	if decrypted != plaintext {
		t.Errorf("GM round-trip: got %q, want %q", decrypted, plaintext)
	}

	// GM mode should reject 32-byte keys
	_, err = NewEncryptedDBStore(testMasterKey(), gmProvider)
	if err == nil {
		t.Error("GM mode should reject 32-byte key (expects 16)")
	}
}

func TestEncryptedV1BackwardCompat(t *testing.T) {
	enc, _ := NewEncryptedDBStore(testMasterKey(), testCryptoProvider())

	// Encrypt with v2, then verify the prefix
	encrypted, _ := enc.Encrypt("legacy-test")
	if !strings.HasPrefix(encrypted, "enc2://") {
		t.Fatal("expected enc2:// prefix")
	}

	// Manually craft a v1 ciphertext to simulate legacy data
	key := make([]byte, 32)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	for i := range nonce {
		nonce[i] = byte(i)
	}
	sealed := aead.Seal(nonce, nonce, []byte("legacy-value"), nil)
	v1Ciphertext := "enc://" + base64.StdEncoding.EncodeToString(sealed)

	// Decrypt should handle v1 format
	decrypted, err := enc.Decrypt(v1Ciphertext)
	if err != nil {
		t.Fatalf("decrypt v1: %v", err)
	}
	if decrypted != "legacy-value" {
		t.Errorf("v1 decrypt: got %q, want %q", decrypted, "legacy-value")
	}
}
