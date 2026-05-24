package secret

import (
	"context"
	"fmt"
	"os"
)

// EnvSecretStore resolves secrets from environment variables.
type EnvSecretStore struct{}

func NewEnvSecretStore() *EnvSecretStore {
	return &EnvSecretStore{}
}

func (s *EnvSecretStore) Name() string { return "env" }

func (s *EnvSecretStore) GetSecret(_ context.Context, key string) (string, error) {
	val, ok := os.LookupEnv(key)
	if !ok || val == "" {
		return "", fmt.Errorf("env variable %s not set or empty", key)
	}
	return val, nil
}
