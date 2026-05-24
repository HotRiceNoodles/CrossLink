package secret

import "context"

// SecretStore is the interface for secret backends.
type SecretStore interface {
	Name() string
	GetSecret(ctx context.Context, key string) (string, error)
}
