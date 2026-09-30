package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSecretsTestDB(t *testing.T, watchInterval time.Duration) *gorm.DB {
	t.Helper()
	if watchInterval <= 0 {
		// No background watcher touching this DB concurrently — plain
		// :memory: is fine (and avoids Windows file-handle cleanup issues).
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		require.NoError(t, db.AutoMigrate(&model.SystemSetting{}, &model.Provider{}))
		return db
	}
	// File-based (not :memory:) — the watcher goroutine and the test poll on
	// separate pooled connections, and an in-memory SQLite DB is
	// per-connection, so a second pooled conn would see no tables.
	// The caller must let the watcher drain and close the pool before
	// TempDir cleanup (Windows refuses to delete open files).
	path := filepath.Join(t.TempDir(), "secrets_test.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SystemSetting{}, &model.Provider{}))
	t.Cleanup(func() {
		time.Sleep(2 * watchInterval) // let a tick-in-flight observe ctx.Done
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return db
}

func randomKeyBase64(t *testing.T, size int) string {
	t.Helper()
	raw := make([]byte, size)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

// TestEncryptionWatcher_LazyActivation covers the fresh-install path the
// setup wizard targets: process boots keyless (EncStore nil), the wizard
// writes the encryption_key row, and the watcher activates encryption —
// registering the store in the resolver and encrypting plaintext provider
// secrets — without a restart.
func TestEncryptionWatcher_LazyActivation(t *testing.T) {
	db := setupSecretsTestDB(t, 50*time.Millisecond)
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)

	resolver := secret.NewSecretResolver(0)
	resolver.Register(secret.NewEnvSecretStore())

	// Plaintext provider exists before any key.
	require.NoError(t, db.Create(&model.Provider{
		Name: "p1", DisplayName: "P1", AdapterType: "openai",
		BaseURL: "https://1.2.4.8", APIKey: "sk-plaintext",
	}).Error)

	activeKey := ""
	activeKeyPtr := &activeKey
	registered := false
	registerEnc := func(store *secret.EncryptedDBStore) {
		resolver.Register(store)
		resolver.Register(store.AsV2())
		registered = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runEncryptionWatcher(ctx, db, resolver, nil, activeKeyPtr, cp, &Extensions{}, registerEnc, 50*time.Millisecond)

	// Key appears (as if written by the setup wizard).
	require.NoError(t, db.Create(&model.SystemSetting{Key: "encryption_key", Value: randomKeyBase64(t, cp.CipherKeySize())}).Error)

	// The provider secret gets encrypted within a few intervals.
	deadline := time.Now().Add(5 * time.Second)
	encrypted := ""
	for time.Now().Before(deadline) {
		var p model.Provider
		require.NoError(t, db.Where("name = ?", "p1").First(&p).Error)
		if len(p.APIKey) > 7 && (p.APIKey[:6] == "enc://" || p.APIKey[:7] == "enc2://") {
			encrypted = p.APIKey
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NotEmpty(t, encrypted, "plaintext provider secret must be encrypted after key activation")
	assert.True(t, registered, "enc store must be registered into the resolver")
	assert.NotEqual(t, "", *activeKeyPtr, "active key pointer tracks the activated key")
}

// TestEncryptionWatcher_HotReload covers the multi-instance rotation path:
// store active at startup, another instance changes the row.
func TestEncryptionWatcher_HotReload(t *testing.T) {
	db := setupSecretsTestDB(t, 50*time.Millisecond)
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)

	key1 := randomKeyBase64(t, cp.CipherKeySize())
	key2 := randomKeyBase64(t, cp.CipherKeySize())

	resolver := secret.NewSecretResolver(0)
	store, err := secret.NewEncryptedDBStore(key1, cp)
	require.NoError(t, err)
	resolver.Register(store)
	resolver.Register(store.AsV2())

	require.NoError(t, db.Create(&model.SystemSetting{Key: "encryption_key", Value: key1}).Error)

	activeKey := key1
	activeKeyPtr := &activeKey
	registerEnc := func(s *secret.EncryptedDBStore) { /* already registered */ }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runEncryptionWatcher(ctx, db, resolver, store, activeKeyPtr, cp, &Extensions{}, registerEnc, 50*time.Millisecond)

	// Rotate the row.
	require.NoError(t, db.Model(&model.SystemSetting{}).Where("key = ?", "encryption_key").Update("value", key2).Error)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if *activeKeyPtr == key2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Equal(t, key2, *activeKeyPtr, "active key must hot-reload to the rotated value")
}

// TestBuildSecrets_KeylessFreshInstall ensures the bundle shape when no key
// exists anywhere (the wizard's starting state): plaintext mode, no crash.
func TestBuildSecrets_KeylessFreshInstall(t *testing.T) {
	db := setupSecretsTestDB(t, 0)
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)
	cfg := &config.Config{}

	bundle := buildSecrets(db, cfg, &Extensions{}, cp, nil)
	defer bundle.CleanupCancel()
	assert.Nil(t, bundle.EncStore)
	assert.Equal(t, "", *bundle.ActiveKeyPtr)
}
