package configio

import (
	"encoding/json"
	"fmt"

	"github.com/crosslink/internal/secret"
)

// resolveForExport prepares a stored secret value for the plaintext export file:
//   - enc:// and enc2:// references are decrypted to plaintext via encStore
//   - env://, file://, vault:// and other external references are preserved as-is
//   - plaintext values are preserved as-is
//
// This intentionally does NOT use SecretResolver.Resolve, which would resolve
// env:// to its current environment value (leaking the secret and losing the
// reference — the reference must survive cross-instance migration).
func resolveForExport(encStore *secret.EncryptedDBStore, val string) (string, error) {
	if !secret.IsReference(val) {
		return val, nil // plaintext or empty
	}
	scheme, _, ok := secret.ParseScheme(val)
	if !ok {
		return val, nil
	}
	if scheme == "enc" || scheme == "enc2" {
		if encStore == nil {
			return "", fmt.Errorf("encrypted secret %q present but no CL_ENCRYPTION_KEY configured", val)
		}
		return encStore.Decrypt(val)
	}
	return val, nil // env:// file:// vault:// etc. — preserve reference
}

// resolveForImport prepares an exported (plaintext) value for storage in the
// target DB:
//   - external references (env:// ...) are stored verbatim — the target runtime
//     resolves them
//   - plaintext values are re-encrypted under the target encStore (if configured),
//     or stored as plaintext when the target runs without CL_ENCRYPTION_KEY
func resolveForImport(encStore *secret.EncryptedDBStore, val string) (string, error) {
	if val == "" {
		return "", nil
	}
	if secret.IsReference(val) {
		return val, nil // env:// etc. — store as-is
	}
	if encStore == nil {
		return val, nil // plaintext deployment
	}
	return encStore.Encrypt(val)
}

// resolveExtraConfigForExport walks the extra_config map, decrypting only the
// sensitive enc://-style fields and leaving env:// references and non-sensitive
// fields untouched. Returns a new map suitable for YAML serialization.
func resolveExtraConfigForExport(encStore *secret.EncryptedDBStore, raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode extra_config: %w", err)
	}
	for k, v := range m {
		if !secret.IsSensitiveField(k) {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		resolved, err := resolveForExport(encStore, s)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", k, err)
		}
		m[k] = resolved
	}
	return m, nil
}

// resolveExtraConfigForImport walks the exported extra_config map, re-encrypting
// sensitive plaintext fields under the target encStore. env:// references are
// preserved. Returns JSON suitable for the ProviderModel.ExtraConfig column.
func resolveExtraConfigForImport(encStore *secret.EncryptedDBStore, m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	for k, v := range m {
		if !secret.IsSensitiveField(k) {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		resolved, err := resolveForImport(encStore, s)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", k, err)
		}
		m[k] = resolved
	}
	return json.Marshal(m)
}
