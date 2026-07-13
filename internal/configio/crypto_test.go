package configio

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	plaintext := []byte("version: \"1\"\nproviders:\n  - name: openai\n    api_key: sk-test\n")
	blob, err := Encrypt(plaintext, "correct horse battery")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(blob, plaintext) {
		t.Fatal("ciphertext must not contain plaintext")
	}
	if string(blob[:len(magicV1)]) != magicV1 {
		t.Fatalf("bad magic header: %q", blob[:len(magicV1)])
	}
	got, err := Decrypt(blob, "correct horse battery")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch\ngot:  %s\nwant: %s", got, plaintext)
	}
}

func TestDecryptWrongPassphrase(t *testing.T) {
	blob, err := Encrypt([]byte("secret"), "right-password")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, err = Decrypt(blob, "wrong-password")
	if !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("expected ErrInvalidPassphrase, got %v", err)
	}
}

func TestDecryptCorruptedFile(t *testing.T) {
	blob, err := Encrypt([]byte("secret"), "pw")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip a ciphertext byte — AEAD tag must fail Open.
	blob[len(blob)-1] ^= 0xFF
	_, err = Decrypt(blob, "pw")
	if !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("expected ErrInvalidPassphrase on tamper, got %v", err)
	}
}

func TestDecryptBadMagic(t *testing.T) {
	// Valid length but wrong magic.
	blob := make([]byte, headerLen+16)
	if _, err := Decrypt(blob, "pw"); err == nil {
		t.Fatal("expected error for bad magic, got nil")
	}
}

func TestDecryptTooShort(t *testing.T) {
	_, err := Decrypt([]byte("short"), "pw")
	if !errors.Is(err, ErrInvalidPassphrase) {
		t.Fatalf("expected ErrInvalidPassphrase for short input, got %v", err)
	}
}
