package admin

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/service"
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

func TestContentLogGetUpdate(t *testing.T) {
	db := setupSystemTestDB(t)
	usageSvc := service.NewUsageService(nil)
	h := NewSystemHandler(db, nil, config.AdminConfig{}, usageSvc, nil, nil, nil, nil)

	// GET: defaults to disabled when no row exists.
	c, w := newTestContext(t, "GET", "/admin/api/system/content-log", nil)
	h.GetContentLog(c)
	require.Equal(t, 200, w.Code)
	var resp struct {
		Data struct {
			Enabled bool `json:"enabled"`
		} `json:"data"`
	}
	decodeResponse(t, w, &resp)
	assert.False(t, resp.Data.Enabled)

	// PUT {"enabled": true} → 200, DB row persisted, runtime state hot-updated.
	c, w = newTestContext(t, "PUT", "/admin/api/system/content-log", map[string]any{"enabled": true})
	h.UpdateContentLog(c)
	require.Equal(t, 200, w.Code)
	var row model.SystemSetting
	require.NoError(t, db.Where("key = ?", "log_content").First(&row).Error)
	assert.Equal(t, "true", row.Value)
	assert.True(t, usageSvc.IsContentLogEnabled())

	// GET now reflects the enabled state.
	c, w = newTestContext(t, "GET", "/admin/api/system/content-log", nil)
	h.GetContentLog(c)
	require.Equal(t, 200, w.Code)
	decodeResponse(t, w, &resp)
	assert.True(t, resp.Data.Enabled)

	// PUT with missing field → 400.
	c, w = newTestContext(t, "PUT", "/admin/api/system/content-log", map[string]any{})
	h.UpdateContentLog(c)
	assert.Equal(t, 400, w.Code)

	// PUT with non-boolean value → 400.
	c, w = newTestContext(t, "PUT", "/admin/api/system/content-log", map[string]any{"enabled": "yes"})
	h.UpdateContentLog(c)
	assert.Equal(t, 400, w.Code)
}
