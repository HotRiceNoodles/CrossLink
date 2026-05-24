package crypto

import (
	stdcrypto "crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// StandardProvider implements CryptoProvider using standard algorithms (SHA-256, RSA).
type StandardProvider struct{}

func (p *StandardProvider) Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

func (p *StandardProvider) HashHex(data []byte) string {
	return hex.EncodeToString(p.Hash(data))
}

func (p *StandardProvider) HMAC(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func (p *StandardProvider) HMACHex(key, data []byte) string {
	return hex.EncodeToString(p.HMAC(key, data))
}

func (p *StandardProvider) CipherKeySize() int { return 32 } // AES-256

func (p *StandardProvider) Sign(privKeyPEM string, data []byte) ([]byte, error) {
	key, err := parseRSAPrivateKey(privKeyPEM)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	return rsa.SignPKCS1v15(rand.Reader, key, stdcrypto.SHA256, hash[:])
}

func (p *StandardProvider) Verify(pubKeyPEM string, data []byte, signature []byte) error {
	key, err := parseRSAPublicKey(pubKeyPEM)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	return rsa.VerifyPKCS1v15(key, stdcrypto.SHA256, hash[:], signature)
}

func (p *StandardProvider) GenerateKeyPair() (privKeyPEM string, pubKeyPEM string, kid string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", "", fmt.Errorf("generate RSA key: %w", err)
	}

	kidBytes := make([]byte, 16)
	if _, err := rand.Read(kidBytes); err != nil {
		return "", "", "", fmt.Errorf("generate kid: %w", err)
	}
	kid = hex.EncodeToString(kidBytes)

	privDER := x509.MarshalPKCS1PrivateKey(key)
	privKeyPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privDER,
	}))

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", "", fmt.Errorf("marshal public key: %w", err)
	}
	pubKeyPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	}))

	return privKeyPEM, pubKeyPEM, kid, nil
}

func (p *StandardProvider) JWTSigningMethod() jwt.SigningMethod {
	return jwt.SigningMethodHS256
}

func (p *StandardProvider) Algorithms() AlgorithmSet {
	return AlgorithmSet{
		Hash:    AlgoSHA256,
		HMAC:    AlgoHMACSHA256,
		Signing: AlgoRSA,
	}
}

func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse RSA private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not RSA")
	}
	return rsaKey, nil
}

func parseRSAPublicKey(pemStr string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse RSA public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key is not RSA")
	}
	return rsaPub, nil
}
