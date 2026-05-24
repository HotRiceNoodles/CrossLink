package secret

import "strings"

// IsReference returns true if s looks like a URI-style secret reference (contains "://").
func IsReference(s string) bool {
	if s == "" || !strings.Contains(s, "://") {
		return false
	}
	scheme := s[:strings.Index(s, "://")]
	if scheme == "" {
		return false
	}
	return scheme != "http" && scheme != "https"
}

// ParseScheme splits a URI reference into scheme and key path.
// Returns ("", "", false) if the string is not a reference.
func ParseScheme(ref string) (scheme, keyPath string, ok bool) {
	idx := strings.Index(ref, "://")
	if idx <= 0 {
		return "", "", false
	}
	return ref[:idx], ref[idx+3:], true
}

// sensitiveFields lists extra_config JSONB keys that should be treated as secrets.
var sensitiveFields = map[string]bool{
	"access_key_id":       true,
	"secret_access_key":   true,
	"session_token":       true,
	"service_account_key": true,
	"access_token":        true,
	"api_key":             true,
	"client_secret":       true,
	"private_key":         true,
	"password":            true,
	"auth_token":          true,
	"bearer_token":        true,
}

// IsSensitiveField reports whether an extra_config key should be treated as a secret.
func IsSensitiveField(key string) bool {
	return sensitiveFields[key]
}

// SensitiveFields is kept for backward compatibility. Prefer IsSensitiveField.
var SensitiveFields = sensitiveFields
