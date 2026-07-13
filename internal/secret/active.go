package secret

import (
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

// InitActiveEncryption resolves the active encryption key and constructs an
// EncryptedDBStore. Key precedence: DB system_setting "encryption_key"
// (post-rotation) takes priority over the config key; if the DB key is invalid,
// it falls back to the config key.
//
// Returns:
//   - (nil, "", nil) when no encryption key is configured (plaintext deployment)
//   - (store, activeKey, nil) on success
//   - (nil, "", err) when a key is present but all candidates are invalid
//
// The resolved activeKey is returned so callers (e.g. the server's key-rotation
// watcher) can track the in-use key. Shared by app.buildSecrets and the
// config-export/config-import CLI tools to keep key resolution in one place.
func InitActiveEncryption(db *gorm.DB, cfgKey string, cp crypto.CryptoProvider) (*EncryptedDBStore, string, error) {
	activeKey := cfgKey
	var dbKey model.SystemSetting
	if result := db.Where("key = ?", "encryption_key").First(&dbKey); result.Error == nil && dbKey.Value != "" {
		activeKey = dbKey.Value
	}
	if activeKey == "" {
		return nil, "", nil
	}

	store, err := NewEncryptedDBStore(activeKey, cp)
	if err != nil {
		// DB key invalid: fall back to config key if different.
		if cfgKey != "" && cfgKey != activeKey {
			activeKey = cfgKey
			store, err = NewEncryptedDBStore(activeKey, cp)
		}
		if err != nil {
			return nil, "", err
		}
	}
	return store, activeKey, nil
}
