package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	"github.com/crosslink/internal/crypto"
	gmsm4 "github.com/tjfoc/gmsm/sm4"
)

// EncryptedDBStore provides AES-256-GCM or SM4-GCM encryption/decryption for secrets stored in the database.
//
// Format v1: "enc://" prefix, always AES-256-GCM.
// Format v2: "enc2://" prefix, AES-256-GCM (standard mode) or SM4-GCM (GM mode).
type EncryptedDBStore struct {
	mu        sync.RWMutex
	masterKey []byte // AES: 32 bytes, SM4: 16 bytes
	cp        crypto.CryptoProvider
}

func NewEncryptedDBStore(masterKeyBase64 string, cp crypto.CryptoProvider) (*EncryptedDBStore, error) {
	key, err := base64.StdEncoding.DecodeString(masterKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("decode base64 key: %w", err)
	}
	expectedLen := cp.CipherKeySize()
	if len(key) != expectedLen {
		return nil, fmt.Errorf("encryption key must be %d bytes, got %d", expectedLen, len(key))
	}
	return &EncryptedDBStore{masterKey: key, cp: cp}, nil
}

func (s *EncryptedDBStore) Name() string { return "enc" }

func (s *EncryptedDBStore) GetSecret(_ context.Context, key string) (string, error) {
	return s.Decrypt("enc://" + key)
}

// AsV2 returns a SecretStore handle that resolves enc2:// references.
// The wrapper reconstructs the "enc2://" prefix so Decrypt uses the correct cipher (v2/provider).
func (s *EncryptedDBStore) AsV2() SecretStore {
	return &encV2Store{store: s}
}

type encV2Store struct {
	store *EncryptedDBStore
}

func (s *encV2Store) Name() string { return "enc2" }

func (s *encV2Store) GetSecret(_ context.Context, key string) (string, error) {
	return s.store.Decrypt("enc2://" + key)
}

// IsEncrypted returns true if s starts with "enc://" or "enc2://".
func (s *EncryptedDBStore) IsEncrypted(s2 string) bool {
	return strings.HasPrefix(s2, "enc://") || strings.HasPrefix(s2, "enc2://")
}

// Encrypt encrypts plaintext using the current provider's cipher (v2 format).
// Returns "enc2://base64(nonce||ciphertext||tag)".
func (s *EncryptedDBStore) Encrypt(plaintext string) (string, error) {
	s.mu.RLock()
	key := s.masterKey
	s.mu.RUnlock()

	block, err := s.newBlock(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return "enc2://" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt decrypts an "enc://..." or "enc2://..." string back to plaintext.
// v1 (enc://) always uses AES-256-GCM for backward compatibility.
// v2 (enc2://) uses the current provider's cipher.
func (s *EncryptedDBStore) Decrypt(encrypted string) (string, error) {
	s.mu.RLock()
	key := s.masterKey
	s.mu.RUnlock()

	var useV1 bool
	var raw string
	if strings.HasPrefix(encrypted, "enc2://") {
		raw = strings.TrimPrefix(encrypted, "enc2://")
	} else {
		raw = strings.TrimPrefix(encrypted, "enc://")
		useV1 = true
	}

	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}

	var block cipher.Block
	if useV1 {
		// Legacy: always AES-256-GCM with a 32-byte key
		block, err = aes.NewCipher(key)
	} else {
		block, err = s.newBlock(key)
	}
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}
	nonceSize := aead.NonceSize()
	if len(data) < nonceSize+aead.Overhead() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

// SetMasterKey replaces the encryption key. Used during key rotation.
func (s *EncryptedDBStore) SetMasterKey(masterKeyBase64 string) error {
	key, err := base64.StdEncoding.DecodeString(masterKeyBase64)
	if err != nil {
		return fmt.Errorf("decode base64 key: %w", err)
	}
	expectedLen := s.cp.CipherKeySize()
	if len(key) != expectedLen {
		return fmt.Errorf("encryption key must be %d bytes, got %d", expectedLen, len(key))
	}
	s.mu.Lock()
	s.masterKey = key
	s.mu.Unlock()
	return nil
}

// newBlock creates the appropriate cipher.Block based on the crypto provider.
func (s *EncryptedDBStore) newBlock(key []byte) (cipher.Block, error) {
	if s.cp.CipherKeySize() == 16 {
		return gmsm4.NewCipher(key)
	}
	return aes.NewCipher(key)
}
