package crypto

import (
	"testing"
)

// SM3 test vectors from GM/T 0004-2012 and RFC 9579.

func TestSM3_EmptyString(t *testing.T) {
	p, _ := NewProvider("gm")
	// GM/T 0004-2012: SM3("") = 1ab21d8355cfa17f8e61194831e81a8f22bec8c728fefb747ed035eb5082aa2b
	got := p.HashHex([]byte(""))
	want := "1ab21d8355cfa17f8e61194831e81a8f22bec8c728fefb747ed035eb5082aa2b"
	if got != want {
		t.Errorf("SM3(\"\") = %s, want %s", got, want)
	}
}

func TestSM3_ShortMessage(t *testing.T) {
	p, _ := NewProvider("gm")
	// SM3("abc") per GM/T 0004-2012
	got := p.HashHex([]byte("abc"))
	want := "66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0"
	if got != want {
		t.Errorf("SM3(\"abc\") = %s, want %s", got, want)
	}
}

func TestSM3_OutputLength(t *testing.T) {
	p, _ := NewProvider("gm")
	hash := p.Hash([]byte("test"))
	if len(hash) != 32 {
		t.Errorf("SM3 output length = %d bytes, want 32", len(hash))
	}
}

func TestSM3_HexLength(t *testing.T) {
	p, _ := NewProvider("gm")
	hex := p.HashHex([]byte("test"))
	if len(hex) != 64 {
		t.Errorf("SM3 hex length = %d chars, want 64", len(hex))
	}
}

func TestSM3_Deterministic(t *testing.T) {
	p, _ := NewProvider("gm")
	data := []byte("deterministic test")
	h1 := p.HashHex(data)
	h2 := p.HashHex(data)
	if h1 != h2 {
		t.Error("SM3 should be deterministic")
	}
}

func TestSM3_DifferentInputs(t *testing.T) {
	p, _ := NewProvider("gm")
	h1 := p.HashHex([]byte("input1"))
	h2 := p.HashHex([]byte("input2"))
	if h1 == h2 {
		t.Error("SM3 should produce different hashes for different inputs")
	}
}

func TestSM3_HMAC(t *testing.T) {
	p, _ := NewProvider("gm")
	key := []byte("secret")
	data := []byte("message")

	// HMAC-SM3 should produce 32-byte output
	hmac := p.HMAC(key, data)
	if len(hmac) != 32 {
		t.Errorf("HMAC-SM3 output length = %d, want 32", len(hmac))
	}

	// HMAC should be deterministic
	hex1 := p.HMACHex(key, data)
	hex2 := p.HMACHex(key, data)
	if hex1 != hex2 {
		t.Error("HMAC-SM3 should be deterministic")
	}

	// Different keys should produce different HMACs
	hex3 := p.HMACHex([]byte("other"), data)
	if hex1 == hex3 {
		t.Error("HMAC-SM3 should differ with different keys")
	}
}

// SM2 signature round-trip tests (GM/T 0003-2012)

func TestSM2_SignVerifyRoundTrip(t *testing.T) {
	p, _ := NewProvider("gm")
	privPEM, pubPEM, kid, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if kid == "" {
		t.Error("kid should not be empty")
	}
	if len(privPEM) == 0 || len(pubPEM) == 0 {
		t.Error("PEM keys should not be empty")
	}

	data := []byte("test message for SM2 signature")
	sig, err := p.Sign(privPEM, data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) == 0 {
		t.Error("signature should not be empty")
	}

	if err := p.Verify(pubPEM, data, sig); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestSM2_SignVerifyWrongData(t *testing.T) {
	p, _ := NewProvider("gm")
	privPEM, pubPEM, _, _ := p.GenerateKeyPair()

	sig, _ := p.Sign(privPEM, []byte("original"))
	if err := p.Verify(pubPEM, []byte("tampered"), sig); err == nil {
		t.Error("Verify should fail with wrong data")
	}
}

func TestSM2_SignVerifyWrongKey(t *testing.T) {
	p, _ := NewProvider("gm")
	priv1, _, _, _ := p.GenerateKeyPair()
	_, pub2, _, _ := p.GenerateKeyPair()

	sig, _ := p.Sign(priv1, []byte("test"))
	if err := p.Verify(pub2, []byte("test"), sig); err == nil {
		t.Error("Verify should fail with wrong public key")
	}
}

func TestSM2_KeyPEMFormat(t *testing.T) {
	p, _ := NewProvider("gm")
	privPEM, pubPEM, _, _ := p.GenerateKeyPair()

	if len(privPEM) < 20 || privPEM[:5] != "-----" {
		t.Errorf("private key should be PEM format, got: %s...", privPEM[:min(20, len(privPEM))])
	}
	if len(pubPEM) < 20 || pubPEM[:5] != "-----" {
		t.Errorf("public key should be PEM format, got: %s...", pubPEM[:min(20, len(pubPEM))])
	}
}

// SM4 key size and cipher tests (GM/T 0002-2012)

func TestSM4_KeySize(t *testing.T) {
	p, _ := NewProvider("gm")
	if p.CipherKeySize() != 16 {
		t.Errorf("SM4 key size = %d, want 16", p.CipherKeySize())
	}
}

func TestSM4_VsAESKeySize(t *testing.T) {
	gm, _ := NewProvider("gm")
	std, _ := NewProvider("standard")
	if gm.CipherKeySize() == std.CipherKeySize() {
		t.Error("SM4 (16 bytes) and AES (32 bytes) should have different key sizes")
	}
}

// Algorithm identification tests

func TestGMAlgorithms(t *testing.T) {
	p, _ := NewProvider("gm")
	a := p.Algorithms()
	if a.Hash != AlgoSM3 {
		t.Errorf("GM Hash = %s, want %s", a.Hash, AlgoSM3)
	}
	if a.HMAC != AlgoHMACSM3 {
		t.Errorf("GM HMAC = %s, want %s", a.HMAC, AlgoHMACSM3)
	}
	if a.Signing != AlgoSM2 {
		t.Errorf("GM Signing = %s, want %s", a.Signing, AlgoSM2)
	}
}

func TestStandardAlgorithms(t *testing.T) {
	p, _ := NewProvider("standard")
	a := p.Algorithms()
	if a.Hash != AlgoSHA256 {
		t.Errorf("Standard Hash = %s, want %s", a.Hash, AlgoSHA256)
	}
	if a.HMAC != AlgoHMACSHA256 {
		t.Errorf("Standard HMAC = %s, want %s", a.HMAC, AlgoHMACSHA256)
	}
	if a.Signing != AlgoRSA {
		t.Errorf("Standard Signing = %s, want %s", a.Signing, AlgoRSA)
	}
}

// RSA round-trip tests (standard mode)

func TestRSA_SignVerifyRoundTrip(t *testing.T) {
	p, _ := NewProvider("standard")
	privPEM, pubPEM, kid, err := p.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if kid == "" {
		t.Error("kid should not be empty")
	}

	data := []byte("test message for RSA signature")
	sig, err := p.Sign(privPEM, data)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := p.Verify(pubPEM, data, sig); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestRSA_SignVerifyWrongData(t *testing.T) {
	p, _ := NewProvider("standard")
	privPEM, pubPEM, _, _ := p.GenerateKeyPair()
	sig, _ := p.Sign(privPEM, []byte("original"))
	if err := p.Verify(pubPEM, []byte("tampered"), sig); err == nil {
		t.Error("RSA Verify should fail with wrong data")
	}
}

func TestRSA_KeySize(t *testing.T) {
	p, _ := NewProvider("standard")
	if p.CipherKeySize() != 32 {
		t.Errorf("AES key size = %d, want 32", p.CipherKeySize())
	}
}

// Factory tests

func TestFactory_ValidModes(t *testing.T) {
	for _, mode := range []string{"standard", "", "gm"} {
		p, err := NewProvider(mode)
		if err != nil {
			t.Errorf("NewProvider(%q): %v", mode, err)
		}
		if p == nil {
			t.Errorf("NewProvider(%q) returned nil", mode)
		}
	}
}

func TestFactory_InvalidMode(t *testing.T) {
	_, err := NewProvider("invalid")
	if err == nil {
		t.Error("expected error for invalid mode")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
