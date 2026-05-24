package admin

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// parseID
// ---------------------------------------------------------------------------

func TestParseID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int64
	}{
		{"positive integer", "42", 42},
		{"zero", "0", 0},
		{"negative integer", "-7", -7},
		{"large int64", "9223372036854775807", 9223372036854775807},
		{"empty string returns 0", "", 0},
		{"non-numeric returns 0", "abc", 0},
		{"float returns 0", "3.14", 0},
		{"spaces return 0", " 42 ", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseID(tt.input)
			if got != tt.want {
				t.Errorf("parseID(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// verifyPassword — bcrypt-based password verification
// ---------------------------------------------------------------------------

func TestVerifyPassword_Bcrypt(t *testing.T) {
	t.Run("correct password matches bcrypt hash", func(t *testing.T) {
		hash, err := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("bcrypt failed: %v", err)
		}
		if !verifyPassword("secret123", string(hash)) {
			t.Error("correct password should verify")
		}
	})

	t.Run("wrong password does not match", func(t *testing.T) {
		hash, err := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("bcrypt failed: %v", err)
		}
		if verifyPassword("wrong-password", string(hash)) {
			t.Error("wrong password must not verify")
		}
	})

	t.Run("different passwords produce different hashes", func(t *testing.T) {
		h1, _ := bcrypt.GenerateFromPassword([]byte("pass1"), bcrypt.DefaultCost)
		h2, _ := bcrypt.GenerateFromPassword([]byte("pass2"), bcrypt.DefaultCost)
		if string(h1) == string(h2) {
			t.Error("different passwords should produce different hashes")
		}
	})
}
