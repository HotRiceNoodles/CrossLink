package secret

import (
	"encoding/json"
	"strings"

	"gorm.io/gorm"
)

// CountPlaintextProviderSecrets returns the number of providers whose api_key
// or extra_config sensitive fields are stored as plaintext (not a secret
// reference and not encrypted). Works without an active key store — encrypted
// values are recognized by their enc:// / enc2:// prefixes, mirroring
// EncryptedDBStore.IsEncrypted. Used by the readiness report to detect
// unencrypted provider secrets at rest.
func CountPlaintextProviderSecrets(db *gorm.DB) (int, error) {
	var providers []struct {
		ID          int64  `gorm:"primaryKey"`
		APIKey      string `gorm:"column:api_key"`
		ExtraConfig []byte `gorm:"column:extra_config"`
	}
	if err := db.Table("providers").Find(&providers).Error; err != nil {
		return 0, err
	}

	isEncrypted := func(s string) bool {
		return strings.HasPrefix(s, "enc://") || strings.HasPrefix(s, "enc2://")
	}
	count := 0
	for _, p := range providers {
		plaintext := false
		if p.APIKey != "" && !IsReference(p.APIKey) && !isEncrypted(p.APIKey) {
			plaintext = true
		}
		if !plaintext && len(p.ExtraConfig) > 0 {
			var config map[string]any
			if json.Unmarshal(p.ExtraConfig, &config) == nil {
				for k, v := range config {
					strVal, ok := v.(string)
					if !ok || strVal == "" || !IsSensitiveField(k) {
						continue
					}
					if !IsReference(strVal) && !isEncrypted(strVal) {
						plaintext = true
						break
					}
				}
			}
		}
		if plaintext {
			count++
		}
	}
	return count, nil
}
