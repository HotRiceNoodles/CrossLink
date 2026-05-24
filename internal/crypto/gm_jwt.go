package crypto

import (
	"crypto/rand"
	"fmt"

	gmSm2 "github.com/tjfoc/gmsm/sm2"
	"github.com/golang-jwt/jwt/v5"
)

// SigningMethodSM2SM3 implements jwt.SigningMethod for SM2+SM3.
type SigningMethodSM2SM3 struct{}

var signingMethodSM2SM3 *SigningMethodSM2SM3

func init() {
	signingMethodSM2SM3 = &SigningMethodSM2SM3{}
	jwt.RegisterSigningMethod("SM2SM3", func() jwt.SigningMethod {
		return signingMethodSM2SM3
	})
}

func (m *SigningMethodSM2SM3) Alg() string { return "SM2SM3" }

func (m *SigningMethodSM2SM3) Sign(signingString string, key interface{}) ([]byte, error) {
	var sm2Key *gmSm2.PrivateKey
	switch k := key.(type) {
	case *gmSm2.PrivateKey:
		sm2Key = k
	case string:
		parsed, err := parseSM2PrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("SM2Sign: parse key: %w", err)
		}
		sm2Key = parsed
	default:
		return nil, fmt.Errorf("SM2Sign: invalid key type %T", key)
	}
	return sm2Key.Sign(rand.Reader, []byte(signingString), nil)
}

func (m *SigningMethodSM2SM3) Verify(signingString string, sig []byte, key interface{}) error {
	var sm2Key *gmSm2.PublicKey
	switch k := key.(type) {
	case *gmSm2.PublicKey:
		sm2Key = k
	case string:
		parsed, err := parseSM2PublicKey(k)
		if err != nil {
			return fmt.Errorf("SM2Verify: parse key: %w", err)
		}
		sm2Key = parsed
	default:
		return fmt.Errorf("SM2Verify: invalid key type %T", key)
	}
	if !sm2Key.Verify([]byte(signingString), sig) {
		return jwt.ErrSignatureInvalid
	}
	return nil
}
