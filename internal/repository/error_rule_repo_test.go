package repository

import (
	"context"
	"testing"

	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupErrorRuleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ErrorClassificationRule{}))
	return db
}

func TestErrorRuleRepo_CRUDAndListEnabled(t *testing.T) {
	db := setupErrorRuleTestDB(t)
	repo := NewErrorRuleRepo(db)
	ctx := context.Background()

	pt := "openai_compatible"
	rules := []*model.ErrorClassificationRule{
		{MatchField: "code", Pattern: "insufficient_quota", Classification: "quota", ProviderType: &pt, Scope: "account", Priority: 100, Enabled: true},
		{MatchField: "status", Pattern: "402", Classification: "quota", Scope: "account", Priority: 50, Enabled: true},
		{MatchField: "code", Pattern: "legacy_disabled", Classification: "quota", Scope: "account", Priority: 200, Enabled: false},
	}
	for _, r := range rules {
		require.NoError(t, repo.Create(ctx, r))
	}

	// ListEnabled excludes disabled, ordered by priority ASC then id ASC.
	enabled, err := repo.ListEnabled(ctx)
	require.NoError(t, err)
	require.Len(t, enabled, 2)
	assert.Equal(t, "402", enabled[0].Pattern)                // priority 50 first
	assert.Equal(t, "insufficient_quota", enabled[1].Pattern) // priority 100 second

	// List includes all (3).
	all, err := repo.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// GetByID.
	got, err := repo.GetByID(ctx, rules[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "insufficient_quota", got.Pattern)

	// Update.
	rules[0].Pattern = "quota_exceeded"
	require.NoError(t, repo.Update(ctx, rules[0]))
	got2, _ := repo.GetByID(ctx, rules[0].ID)
	assert.Equal(t, "quota_exceeded", got2.Pattern)

	// Delete.
	require.NoError(t, repo.Delete(ctx, rules[0].ID))
	_, err = repo.GetByID(ctx, rules[0].ID)
	assert.Error(t, err)
}
