package service

import (
	"context"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupUsageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.UsageLog{}))
	return db
}

// TestUsageService_LogPersistsTemplateID: B2 fix — the template_id that
// ContextAssembler sets on the entry must reach usage_logs so per-template
// analytics work. Before the fix, Log() did not map TemplateID.
func TestUsageService_LogPersistsTemplateID(t *testing.T) {
	db := setupUsageTestDB(t)
	svc := NewUsageService(repository.NewUsageLogRepo(db))

	tplID := int64(42)
	svc.Log(context.Background(), &UsageEntry{
		RouteType:      "openai",
		ModelRequested: "gpt-4o",
		TemplateID:     &tplID,
	})

	var rows []model.UsageLog
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].TemplateID, "template_id must be persisted")
	assert.Equal(t, tplID, *rows[0].TemplateID)
}

// TestUsageService_LogNilTemplateID: requests without a template leave the
// column NULL (backward compatible with all pre-template traffic).
func TestUsageService_LogNilTemplateID(t *testing.T) {
	db := setupUsageTestDB(t)
	svc := NewUsageService(repository.NewUsageLogRepo(db))

	svc.Log(context.Background(), &UsageEntry{RouteType: "openai", ModelRequested: "gpt-4o"})

	var rows []model.UsageLog
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].TemplateID, "template_id must be NULL when no template used")
}
