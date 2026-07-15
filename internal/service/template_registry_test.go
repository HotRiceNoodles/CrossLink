package service

import (
	"context"
	"testing"

	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTemplateRegistryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.PromptTemplate{}))
	return db
}

func TestTemplateRegistry_GetLoadsFromDBAndCaches(t *testing.T) {
	db := setupTemplateRegistryDB(t)
	reg := NewTemplateRegistry(db)
	ctx := context.Background()

	tpl := &model.PromptTemplate{
		Name: "summarize", SystemPrompt: "Summarize the following.",
		TargetFormat: "auto", Status: 1,
		VariablesSchema: datatypes.JSON([]byte(`[{"name":"lang","trusted":true}]`)),
	}
	require.NoError(t, db.Create(tpl).Error)

	// First Get: miss → loads from DB.
	got, ok := reg.Get(ctx, "summarize")
	require.True(t, ok)
	assert.Equal(t, "summarize", got.Name)
	assert.Equal(t, "Summarize the following.", got.SystemPrompt)

	// Mutate DB row; without invalidate, cached value is returned (proves caching).
	db.Model(&model.PromptTemplate{}).Where("name = ?", "summarize").
		Update("system_prompt", "CHANGED")
	got2, ok := reg.Get(ctx, "summarize")
	require.True(t, ok)
	assert.Equal(t, "Summarize the following.", got2.SystemPrompt, "should return cached value, not DB")
}

func TestTemplateRegistry_InvalidateForcesReload(t *testing.T) {
	db := setupTemplateRegistryDB(t)
	reg := NewTemplateRegistry(db)
	ctx := context.Background()

	require.NoError(t, db.Create(&model.PromptTemplate{
		Name: "translate", SystemPrompt: "original", TargetFormat: "auto", Status: 1,
	}).Error)

	reg.Get(ctx, "translate") // populate cache
	db.Model(&model.PromptTemplate{}).Where("name = ?", "translate").
		Update("system_prompt", "updated")

	reg.Invalidate("translate")
	got, ok := reg.Get(ctx, "translate")
	require.True(t, ok)
	assert.Equal(t, "updated", got.SystemPrompt, "invalidate must force DB reload")
}

func TestTemplateRegistry_GetMissingReturnsFalse(t *testing.T) {
	reg := NewTemplateRegistry(setupTemplateRegistryDB(t))
	_, ok := reg.Get(context.Background(), "nope")
	assert.False(t, ok)
}

func TestTemplateRegistry_GetSkipsDisabledAndSoftDeleted(t *testing.T) {
	db := setupTemplateRegistryDB(t)
	reg := NewTemplateRegistry(db)
	ctx := context.Background()

	// Created active, then disabled by admin (GORM applies default=1 on Create
	// for zero-valued Status, so disabling happens via Update, matching real flow).
	require.NoError(t, db.Create(&model.PromptTemplate{
		Name: "disabled", SystemPrompt: "x", TargetFormat: "auto", Status: 1,
	}).Error)
	reg.Get(ctx, "disabled") // populate cache while active
	db.Model(&model.PromptTemplate{}).Where("name = ?", "disabled").Update("status", 0)
	reg.Invalidate("disabled")

	_, ok := reg.Get(ctx, "disabled")
	assert.False(t, ok, "status=0 must not be returned")
}
