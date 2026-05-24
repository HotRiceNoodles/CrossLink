package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestNewProvider(t *testing.T) {
	tests := []struct {
		mode    string
		wantErr bool
	}{
		{"standard", false},
		{"", false},
		{"gm", false},
		{"invalid", true},
	}
	for _, tt := range tests {
		p, err := NewProvider(tt.mode)
		if (err != nil) != tt.wantErr {
			t.Errorf("NewProvider(%q) error = %v, wantErr %v", tt.mode, err, tt.wantErr)
		}
		if !tt.wantErr && p == nil {
			t.Errorf("NewProvider(%q) returned nil", tt.mode)
		}
	}
}

func TestStandardProvider_Hash(t *testing.T) {
	p, _ := NewProvider("standard")
	data := []byte("hello world")

	// Verify Hash matches direct SHA-256
	expected := sha256.Sum256(data)
	got := p.Hash(data)
	if hex.EncodeToString(got) != hex.EncodeToString(expected[:]) {
		t.Errorf("StandardProvider.Hash() = %x, want %x", got, expected)
	}
}

func TestStandardProvider_HashHex(t *testing.T) {
	p, _ := NewProvider("standard")
	got := p.HashHex([]byte("test"))
	expected := sha256.Sum256([]byte("test"))
	want := hex.EncodeToString(expected[:])
	if got != want {
		t.Errorf("StandardProvider.HashHex() = %s, want %s", got, want)
	}
}

func TestStandardProvider_HMAC(t *testing.T) {
	p, _ := NewProvider("standard")
	key := []byte("secret")
	data := []byte("message")

	h := hmac.New(sha256.New, key)
	h.Write(data)
	expected := hex.EncodeToString(h.Sum(nil))

	got := p.HMACHex(key, data)
	if got != expected {
		t.Errorf("StandardProvider.HMACHex() = %s, want %s", got, expected)
	}
}

func TestStandardProvider_CipherKeySize(t *testing.T) {
	p, _ := NewProvider("standard")
	if p.CipherKeySize() != 32 {
		t.Errorf("StandardProvider.CipherKeySize() = %d, want 32", p.CipherKeySize())
	}
}

func TestStandardProvider_SignVerify(t *testing.T) {
	p, _ := NewProvider("standard")
	privPEM, pubPEM, _, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error: %v", err)
	}

	data := []byte("test message to sign")
	sig, err := p.Sign(privPEM, data)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	if err := p.Verify(pubPEM, data, sig); err != nil {
		t.Errorf("Verify() error: %v", err)
	}

	// Wrong data should fail
	if err := p.Verify(pubPEM, []byte("wrong data"), sig); err == nil {
		t.Error("Verify() should fail with wrong data")
	}
}

func TestGMProvider_Hash(t *testing.T) {
	p, _ := NewProvider("gm")
	data := []byte("hello world")

	got := p.Hash(data)
	if len(got) != 32 {
		t.Errorf("GMProvider.Hash() length = %d, want 32", len(got))
	}

	// SM3 of empty string: 1ab21d8355cfa17f8e61194831e81a8f22bec8c728fefb747ed035eb5082aa2b
	gotEmpty := p.HashHex([]byte(""))
	if gotEmpty != "1ab21d8355cfa17f8e61194831e81a8f22bec8c728fefb747ed035eb5082aa2b" {
		t.Errorf("GMProvider.HashHex(empty) = %s, want SM3 test vector", gotEmpty)
	}
}

func TestGMProvider_CipherKeySize(t *testing.T) {
	p, _ := NewProvider("gm")
	if p.CipherKeySize() != 16 {
		t.Errorf("GMProvider.CipherKeySize() = %d, want 16", p.CipherKeySize())
	}
}

func TestGMProvider_SignVerify(t *testing.T) {
	p, _ := NewProvider("gm")
	privPEM, pubPEM, kid, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error: %v", err)
	}
	if kid == "" {
		t.Error("GenerateKeyPair() returned empty kid")
	}

	data := []byte("test message to sign with SM2")
	sig, err := p.Sign(privPEM, data)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	if err := p.Verify(pubPEM, data, sig); err != nil {
		t.Errorf("Verify() error: %v", err)
	}

	// Wrong data should fail
	if err := p.Verify(pubPEM, []byte("wrong data"), sig); err == nil {
		t.Error("Verify() should fail with wrong data")
	}
}

func TestGMProvider_JWTSigningMethod(t *testing.T) {
	p, _ := NewProvider("gm")
	method := p.JWTSigningMethod()
	if method.Alg() != "HMACSM3" {
		t.Errorf("GMProvider.JWTSigningMethod().Alg() = %s, want HMACSM3", method.Alg())
	}
}

func TestStandardProvider_Algorithms(t *testing.T) {
	p, _ := NewProvider("standard")
	a := p.Algorithms()
	if a.Hash != AlgoSHA256 {
		t.Errorf("Standard Hash = %s, want %s", a.Hash, AlgoSHA256)
	}
	if a.HMAC != AlgoHMACSHA256 {
		t.Errorf("Standard HMAC = %s, want %s", a.HMAC, AlgoHMACSHA256)
	}
}

func TestGMProvider_Algorithms(t *testing.T) {
	p, _ := NewProvider("gm")
	a := p.Algorithms()
	if a.Hash != AlgoSM3 {
		t.Errorf("GM Hash = %s, want %s", a.Hash, AlgoSM3)
	}
	if a.HMAC != AlgoHMACSM3 {
		t.Errorf("GM HMAC = %s, want %s", a.HMAC, AlgoHMACSM3)
	}
}

func TestGMProvider_HMAC(t *testing.T) {
	p, _ := NewProvider("gm")
	key := []byte("secret")
	data := []byte("message")

	got := p.HMACHex(key, data)
	if got == "" {
		t.Error("GMProvider.HMACHex() returned empty string")
	}
	if len(p.HMAC(key, data)) != 32 {
		t.Errorf("GMProvider.HMAC() length = %d, want 32", len(p.HMAC(key, data)))
	}
}

func TestGMProvider_GenerateKeyPairPEMFormat(t *testing.T) {
	p, _ := NewProvider("gm")
	privPEM, pubPEM, _, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error: %v", err)
	}
	if len(privPEM) == 0 || len(pubPEM) == 0 {
		t.Error("GenerateKeyPair() returned empty PEM")
	}
	// Verify round-trip: parse back what we generated
	sig, err := p.Sign(privPEM, []byte("roundtrip"))
	if err != nil {
		t.Fatalf("Sign() after GenerateKeyPair round-trip error: %v", err)
	}
	if err := p.Verify(pubPEM, []byte("roundtrip"), sig); err != nil {
		t.Errorf("Verify() after Sign round-trip error: %v", err)
	}
}
