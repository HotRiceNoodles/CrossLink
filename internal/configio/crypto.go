// Package configio implements encrypted export/import of CrossLink configuration
// (providers, provider_models, error_classification_rules) as a password-protected
// file. It is consumed by the cmd/config-export and cmd/config-import CLI tools.
package configio

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/scrypt"
)

// Envelope layout (48-byte header):
//
//	magic    8 bytes   "CLCFGv1\n"  (0x43 0x4C 0x43 0x46 0x47 0x76 0x31 0x0A)
//	salt    16 bytes   scrypt salt (crypto/rand)
//	nonce   24 bytes   XChaCha20-Poly1305 nonce (crypto/rand)
//	ciphertext N bytes AEAD.Seal(nil, nonce, yamlPlaintext, magic)
//
// scrypt parameters are fixed constants (not negotiated per file); forward
// compatibility is via the magic version byte. magic is used as AEAD additional
// data for format identification — only well-formed CLCFGv1 inputs decrypt.
const (
	magicV1     = "CLCFGv1\n"
	saltLen     = 16
	nonceLen    = 24
	headerLen   = len(magicV1) + saltLen + nonceLen
	scryptN     = 1 << 17
	scryptR     = 8
	scryptP     = 1
	keyLen      = 32 // XChaCha20-Poly1305 key
)

// ErrInvalidPassphrase is returned by Decrypt when the password is wrong or the
// file is corrupted/tampered. AEAD Open fails identically for both cases by design.
var ErrInvalidPassphrase = errors.New("invalid passphrase or corrupted config file")

// Encrypt encrypts plaintext (typically YAML) under the given password.
func Encrypt(plaintext []byte, password string) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("read salt: %w", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}

	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("init aead: %w", err)
	}

	out := make([]byte, 0, headerLen+len(plaintext)+aead.Overhead())
	out = append(out, magicV1...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, []byte(magicV1))
	return out, nil
}

// Decrypt decrypts a CLCFG envelope produced by Encrypt.
func Decrypt(blob []byte, password string) ([]byte, error) {
	if len(blob) < headerLen {
		return nil, ErrInvalidPassphrase
	}
	if string(blob[:len(magicV1)]) != magicV1 {
		return nil, fmt.Errorf("not a CLCFGv1 config file (bad magic)")
	}
	off := len(magicV1)
	salt := blob[off : off+saltLen]
	off += saltLen
	nonce := blob[off : off+nonceLen]
	off += nonceLen
	ciphertext := blob[off:]

	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("init aead: %w", err)
	}

	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(magicV1))
	if err != nil {
		return nil, ErrInvalidPassphrase
	}
	return plaintext, nil
}
