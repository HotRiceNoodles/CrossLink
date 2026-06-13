package admin

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSystemTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SystemSetting{}))
	return db
}

func TestLoadResilienceConfig_NewKeysDefaultAndOverridden(t *testing.T) {
	db := setupSystemTestDB(t)

	// Defaults apply when keys are unset.
	rc := LoadResilienceConfig(db)
	assert.Equal(t, 1800, rc.PersistentCooldown)
	assert.Equal(t, 5, rc.RetryAfterMin)
	assert.Equal(t, 300, rc.RetryAfterMax)

	// Overriding persistent_cooldown is picked up.
	require.NoError(t, db.Create(&model.SystemSetting{Key: "persistent_cooldown", Value: "600"}).Error)
	rc = LoadResilienceConfig(db)
	assert.Equal(t, 600, rc.PersistentCooldown)
}

func TestRunResilienceRefreshLoop_RunsAndStops(t *testing.T) {
	db := setupSystemTestDB(t)
	health := provider.NewHealthTracker()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunResilienceRefreshLoop(ctx, db, health, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond) // allow the initial apply() to run
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop after cancel")
	}
	assert.True(t, health.IsHealthy("X")) // untouched health is healthy
}
