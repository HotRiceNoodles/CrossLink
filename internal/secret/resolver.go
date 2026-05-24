package secret

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SecretResolver resolves URI-style secret references via registered SecretStore backends.
type SecretResolver struct {
	mu     sync.RWMutex
	stores map[string]SecretStore
	cache  *secretCache
}

func NewSecretResolver(ttl time.Duration) *SecretResolver {
	return &SecretResolver{
		stores: make(map[string]SecretStore),
		cache:  newSecretCache(ttl),
	}
}

// Register adds a SecretStore backend to the resolver.
func (r *SecretResolver) Register(store SecretStore) {
	r.mu.Lock()
	r.stores[store.Name()] = store
	r.mu.Unlock()
}

// Stores returns the list of registered backend names.
func (r *SecretResolver) Stores() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.stores))
	for name := range r.stores {
		names = append(names, name)
	}
	return names
}

// HasStore reports whether a store with the given name is registered.
func (r *SecretResolver) HasStore(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.stores[name]
	return ok
}

// Resolve resolves a secret reference to its plaintext value.
// Empty string returns empty. Plaintext (no "://") is returned as-is.
func (r *SecretResolver) Resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if !IsReference(ref) {
		return ref, nil
	}

	if val, ok := r.cache.Get(ref); ok {
		return val, nil
	}

	scheme, keyPath, ok := ParseScheme(ref)
	if !ok {
		return ref, nil
	}

	r.mu.RLock()
	store, ok := r.stores[scheme]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("no secret store registered for scheme: %s", scheme)
	}

	val, err := store.GetSecret(ctx, keyPath)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}

	r.cache.Set(ref, val)
	return val, nil
}

// ResolveExtraConfigSecrets resolves sensitive field values in a config map.
// Values that are secret references are replaced with their resolved plaintext.
func (r *SecretResolver) ResolveExtraConfigSecrets(ctx context.Context, config map[string]any) error {
	for k, v := range config {
		if !IsSensitiveField(k) {
			continue
		}
		strVal, ok := v.(string)
		if !ok || !IsReference(strVal) {
			continue
		}
		resolved, err := r.Resolve(ctx, strVal)
		if err != nil {
			return fmt.Errorf("resolve extra_config field %s: %w", k, err)
		}
		config[k] = resolved
	}
	return nil
}

// InvalidateCache clears the resolver's in-memory cache.
func (r *SecretResolver) InvalidateCache() {
	r.cache.InvalidateAll()
}
