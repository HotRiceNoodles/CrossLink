package crypto

import "fmt"

// NewProvider creates a CryptoProvider for the given mode ("standard" or "gm").
func NewProvider(mode string) (CryptoProvider, error) {
	switch mode {
	case "standard", "":
		return &StandardProvider{}, nil
	case "gm":
		return &GMProvider{}, nil
	default:
		return nil, fmt.Errorf("unsupported crypto mode: %s", mode)
	}
}
