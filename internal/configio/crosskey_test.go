package configio

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// encStoreWithKey builds an EncryptedDBStore from an arbitrary 32-byte master key,
// so source and target can use DIFFERENT keys (the production cross-instance scenario
// where CL_ENCRYPTION_KEY differs between machines).
func encStoreWithKey(t *testing.T, keyByte byte) *secret.EncryptedDBStore {
	t.Helper()
	cp, _ := crypto.NewProvider("standard")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{keyByte}, 32))
	store, err := secret.NewEncryptedDBStore(key, cp)
	require.NoError(t, err)
	return store
}

// TestCrossEncryptionKeyMigration verifies the headline cross-instance promise:
// export under CL_ENCRYPTION_KEY=A, import under a DIFFERENT key=B, and the
// imported provider's api_key is re-encrypted under B and decryptable there.
//
// This closes the gap that TestBuildAndApplyRoundTrip leaves open (it uses the
// same key for source and target, so it does not prove cross-key migration).
func TestCrossEncryptionKeyMigration(t *testing.T) {
	setup := func() *gorm.DB {
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		require.NoError(t, db.AutoMigrate(&model.Provider{}, &model.ProviderModel{}, &model.ErrorClassificationRule{}))
		return db
	}

	sourceStore := encStoreWithKey(t, 0xAA) // CL_ENCRYPTION_KEY on machine A
	targetStore := encStoreWithKey(t, 0xBB) // DIFFERENT key on machine B
	require.NotEqual(t, sourceStore, targetStore)

	// Sanity: the two stores produce mutually unreadable ciphertexts.
	encUnderA, err := sourceStore.Encrypt("sk-secret")
	require.NoError(t, err)
	_, err = targetStore.Decrypt(encUnderA)
	require.Error(t, err, "target key B must NOT decrypt text encrypted under key A")

	// Seed source DB: provider with api_key encrypted under key A.
	srcDB := setup()
	encAPIKey, err := sourceStore.Encrypt("sk-cross-key-secret")
	require.NoError(t, err)
	require.NoError(t, srcDB.Create(&model.Provider{
		Name: "openai", DisplayName: "OpenAI", AdapterType: "openai_compatible",
		BaseURL: "https://api.openai.com/v1", APIKey: encAPIKey, Status: 1,
	}).Error)

	// Export from source (decrypts under key A → plaintext in bundle).
	bundle, warns, err := BuildBundle(srcDB, sourceStore)
	require.NoError(t, err)
	require.Empty(t, warns)
	require.Equal(t, "sk-cross-key-secret", bundle.Providers[0].APIKey, "export must decrypt to plaintext regardless of key")

	// Import into target DB with key B (re-encrypts plaintext under key B).
	dstDB := setup()
	report, err := ApplyBundle(dstDB, targetStore, bundle, false)
	require.NoError(t, err)
	require.Empty(t, report.Errors)
	require.Equal(t, 1, report.Created.Providers)

	// The target DB's stored api_key must be encrypted under key B (not key A,
	// not plaintext), and must round-trip under key B.
	var tgt model.Provider
	require.NoError(t, dstDB.Where("name = ?", "openai").First(&tgt).Error)
	assert.NotEqual(t, "sk-cross-key-secret", tgt.APIKey, "must not be plaintext")
	assert.NotEqual(t, encAPIKey, tgt.APIKey, "must not be the source ciphertext (that was under key A)")

	dec, err := targetStore.Decrypt(tgt.APIKey)
	require.NoError(t, err, "target store (key B) must decrypt the imported api_key")
	assert.Equal(t, "sk-cross-key-secret", dec, "cross-key migration must preserve the secret value")

	// And the source store (key A) must FAIL to decrypt the target ciphertext —
	// proving the secret is now bound to the target instance's key.
	_, err = sourceStore.Decrypt(tgt.APIKey)
	require.Error(t, err, "source key A must NOT decrypt target ciphertext — secret is now bound to key B")
}
