package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"golang.org/x/crypto/bcrypt"
)

func TestGenerateAndValidateToken(t *testing.T) {
	cfg := config.AdminConfig{
		Username:    "admin",
		Password:    "test",
		JWTSecret:   "test-secret",
		TokenExpiry: 1,
	}

	user := &model.User{ID: 1, Username: "admin", RoleID: 1}
	cp, _ := crypto.NewProvider("standard")
	token, err := GenerateToken(user, model.RoleAdmin, 0, cfg, cp)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("token should not be empty")
	}
}

func TestVerifyPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("testpass"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt hash failed: %v", err)
	}
	if !verifyPassword("testpass", string(hash)) {
		t.Error("should verify correct password against bcrypt hash")
	}
	if verifyPassword("wrongpass", string(hash)) {
		t.Error("should not verify wrong password")
	}
}

func TestIsLegacyHash(t *testing.T) {
	h := sha256.Sum256([]byte("test"))
	shaHash := hex.EncodeToString(h[:])
	if !isLegacyHash(shaHash) {
		t.Error("SHA-256 hash should be detected as legacy")
	}
	if isLegacyHash("$2a$10$abcdefghijklmnop") {
		t.Error("bcrypt hash should not be detected as legacy")
	}
}

func TestVerifyPasswordLegacyUpgrade(t *testing.T) {
	h := sha256.Sum256([]byte("mypassword"))
	legacyHash := hex.EncodeToString(h[:])
	if !verifyPassword("mypassword", legacyHash) {
		t.Error("should verify against legacy SHA-256 hash")
	}
	if verifyPassword("wrongpass", legacyHash) {
		t.Error("should not verify wrong password against legacy hash")
	}
}
