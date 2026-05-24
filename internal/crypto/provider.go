package crypto

import (
	"github.com/golang-jwt/jwt/v5"
)

// Algorithm identifies a specific cryptographic algorithm.
type Algorithm string

const (
	AlgoSHA256     Algorithm = "sha256"
	AlgoSM3        Algorithm = "sm3"
	AlgoHMACSHA256 Algorithm = "hmac-sha256"
	AlgoHMACSM3    Algorithm = "hmac-sm3"
	AlgoRSA        Algorithm = "rsa-2048"
	AlgoSM2        Algorithm = "sm2"
)

// CryptoProvider abstracts application-layer cryptographic operations.
type CryptoProvider interface {
	Hasher
	Signer
	JWTSigningMethod() jwt.SigningMethod
	Algorithms() AlgorithmSet
	CipherKeySize() int
}

// Hasher provides hash and HMAC operations.
type Hasher interface {
	Hash(data []byte) []byte
	HashHex(data []byte) string
	HMAC(key, data []byte) []byte
	HMACHex(key, data []byte) string
}

// Signer provides asymmetric signature operations.
type Signer interface {
	GenerateKeyPair() (privKeyPEM string, pubKeyPEM string, kid string, err error)
	Sign(privKeyPEM string, data []byte) ([]byte, error)
	Verify(pubKeyPEM string, data []byte, signature []byte) error
}

// AlgorithmSet describes which algorithms a provider uses.
type AlgorithmSet struct {
	Hash    Algorithm
	HMAC    Algorithm
	Signing Algorithm
}
