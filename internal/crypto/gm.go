package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"

	gmSm2 "github.com/tjfoc/gmsm/sm2"
	"github.com/tjfoc/gmsm/sm3"
	smx509 "github.com/tjfoc/gmsm/x509"
	"github.com/golang-jwt/jwt/v5"
)

// hmacSM3 is the HMAC-SM3 signing method for Admin JWT in GM mode.
var hmacSM3 *SigningMethodHMACSM3

func init() {
	hmacSM3 = &SigningMethodHMACSM3{}
	jwt.RegisterSigningMethod("HMACSM3", func() jwt.SigningMethod { return hmacSM3 })
}

// SigningMethodHMACSM3 implements jwt.SigningMethod using HMAC-SM3.
type SigningMethodHMACSM3 struct{}

func (m *SigningMethodHMACSM3) Alg() string { return "HMACSM3" }

func (m *SigningMethodHMACSM3) Sign(signingString string, key interface{}) ([]byte, error) {
	keyBytes, ok := key.([]byte)
	if !ok {
		return nil, jwt.ErrSignatureInvalid
	}
	h := hmac.New(sm3.New, keyBytes)
	h.Write([]byte(signingString))
	return h.Sum(nil), nil
}

func (m *SigningMethodHMACSM3) Verify(signingString string, sig []byte, key interface{}) error {
	expected, err := m.Sign(signingString, key)
	if err != nil {
		return err
	}
	if !hmac.Equal(sig, expected) {
		return jwt.ErrSignatureInvalid
	}
	return nil
}

// GMProvider implements CryptoProvider using GM algorithms (SM3, SM2).
type GMProvider struct{}

func (p *GMProvider) Hash(data []byte) []byte {
	h := sm3.New()
	h.Write(data)
	return h.Sum(nil)
}

func (p *GMProvider) HashHex(data []byte) string {
	return hex.EncodeToString(p.Hash(data))
}

func (p *GMProvider) HMAC(key, data []byte) []byte {
	h := hmac.New(sm3.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func (p *GMProvider) HMACHex(key, data []byte) string {
	return hex.EncodeToString(p.HMAC(key, data))
}

func (p *GMProvider) CipherKeySize() int { return 16 } // SM4

func (p *GMProvider) Sign(privKeyPEM string, data []byte) ([]byte, error) {
	key, err := parseSM2PrivateKey(privKeyPEM)
	if err != nil {
		return nil, err
	}
	return key.Sign(rand.Reader, data, nil)
}

func (p *GMProvider) Verify(pubKeyPEM string, data []byte, signature []byte) error {
	key, err := parseSM2PublicKey(pubKeyPEM)
	if err != nil {
		return err
	}
	if !key.Verify(data, signature) {
		return fmt.Errorf("SM2 signature verification failed")
	}
	return nil
}

func (p *GMProvider) GenerateKeyPair() (privKeyPEM string, pubKeyPEM string, kid string, err error) {
	key, err := gmSm2.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", "", fmt.Errorf("generate SM2 key: %w", err)
	}

	kidBytes := make([]byte, 16)
	if _, err := rand.Read(kidBytes); err != nil {
		return "", "", "", fmt.Errorf("generate kid: %w", err)
	}
	kid = hex.EncodeToString(kidBytes)

	privDER, err := smx509.MarshalSm2PrivateKey(key, nil)
	if err != nil {
		return "", "", "", fmt.Errorf("marshal SM2 private key: %w", err)
	}
	privKeyPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "SM2 PRIVATE KEY",
		Bytes: privDER,
	}))

	pubDER, err := smx509.MarshalSm2PublicKey(&key.PublicKey)
	if err != nil {
		return "", "", "", fmt.Errorf("marshal SM2 public key: %w", err)
	}
	pubKeyPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "SM2 PUBLIC KEY",
		Bytes: pubDER,
	}))

	return privKeyPEM, pubKeyPEM, kid, nil
}

func (p *GMProvider) JWTSigningMethod() jwt.SigningMethod {
	return hmacSM3
}

func (p *GMProvider) Algorithms() AlgorithmSet {
	return AlgorithmSet{
		Hash:    AlgoSM3,
		HMAC:    AlgoHMACSM3,
		Signing: AlgoSM2,
	}
}

func parseSM2PrivateKey(pemStr string) (*gmSm2.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}
	// MarshalSm2PrivateKey produces PKCS#8; use PKCS8 parser
	return smx509.ParsePKCS8UnecryptedPrivateKey(block.Bytes)
}

func parseSM2PublicKey(pemStr string) (*gmSm2.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}
	return smx509.ParseSm2PublicKey(block.Bytes)
}
