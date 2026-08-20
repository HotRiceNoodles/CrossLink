package repository

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupPatTokenTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.PatToken{}))
	return db
}

func TestPatTokenRepo_FindByHash(t *testing.T) {
	db := setupPatTokenTestDB(t)
	repo := NewPatTokenRepo(db)
	ctx := context.Background()

	tok := &model.PatToken{
		UserID:    1,
		Name:      "ci",
		TokenHash: "a" + string(make([]byte, 0)) + "b",
		Scopes:    datatypes.JSON(`["budget:read"]`),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, repo.Create(ctx, tok))

	got, err := repo.FindByHash(ctx, tok.TokenHash)
	require.NoError(t, err)
	assert.Equal(t, tok.ID, got.ID)
	assert.Equal(t, int64(1), got.UserID)

	_, err = repo.FindByHash(ctx, "missing")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestPatTokenRepo_ListByUserOrdering(t *testing.T) {
	db := setupPatTokenTestDB(t)
	repo := NewPatTokenRepo(db)
	ctx := context.Background()

	old := &model.PatToken{UserID: 7, Name: "old", TokenHash: "h1", Scopes: datatypes.JSON(`[]`), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now().Add(-time.Hour)}
	new := &model.PatToken{UserID: 7, Name: "new", TokenHash: "h2", Scopes: datatypes.JSON(`[]`), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}
	other := &model.PatToken{UserID: 8, Name: "other", TokenHash: "h3", Scopes: datatypes.JSON(`[]`), ExpiresAt: time.Now().Add(time.Hour)}
	for _, tk := range []*model.PatToken{old, new, other} {
		require.NoError(t, repo.Create(ctx, tk))
	}

	list, err := repo.ListByUser(ctx, 7)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "new", list[0].Name)
	assert.Equal(t, "old", list[1].Name)
}

func TestPatTokenRepo_Revoke(t *testing.T) {
	db := setupPatTokenTestDB(t)
	repo := NewPatTokenRepo(db)
	ctx := context.Background()

	tok := &model.PatToken{UserID: 1, Name: "r", TokenHash: "h", Scopes: datatypes.JSON(`[]`), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, repo.Create(ctx, tok))

	require.NoError(t, repo.Revoke(ctx, tok.ID))
	got, err := repo.GetByID(ctx, tok.ID)
	require.NoError(t, err)
	assert.Equal(t, int16(0), got.Status)
	require.NotNil(t, got.RevokedAt)
}

func TestPatTokenRepo_TouchLastUsedThrottled(t *testing.T) {
	db := setupPatTokenTestDB(t)
	repo := NewPatTokenRepo(db)
	ctx := context.Background()

	// token1: last_used_at NULL -> first touch updates
	tok1 := &model.PatToken{UserID: 1, Name: "t1", TokenHash: "h1", Scopes: datatypes.JSON(`[]`), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, repo.Create(ctx, tok1))
	require.NoError(t, repo.TouchLastUsed(ctx, tok1.ID))
	got1, err := repo.GetByID(ctx, tok1.ID)
	require.NoError(t, err)
	require.NotNil(t, got1.LastUsedAt)

	// token2: last_used_at = now -> touch within 60s window must NOT update
	now := time.Now()
	tok2 := &model.PatToken{UserID: 1, Name: "t2", TokenHash: "h2", Scopes: datatypes.JSON(`[]`), ExpiresAt: time.Now().Add(time.Hour), LastUsedAt: &now}
	require.NoError(t, repo.Create(ctx, tok2))
	require.NoError(t, repo.TouchLastUsed(ctx, tok2.ID))
	got2, err := repo.GetByID(ctx, tok2.ID)
	require.NoError(t, err)
	require.NotNil(t, got2.LastUsedAt)
	assert.WithinDuration(t, now, *got2.LastUsedAt, time.Second, "recent last_used_at should not be refreshed")

	// token3: last_used_at stale (> 60s ago) -> touch updates
	stale := now.Add(-2 * time.Minute)
	tok3 := &model.PatToken{UserID: 1, Name: "t3", TokenHash: "h3", Scopes: datatypes.JSON(`[]`), ExpiresAt: time.Now().Add(time.Hour), LastUsedAt: &stale}
	require.NoError(t, repo.Create(ctx, tok3))
	require.NoError(t, repo.TouchLastUsed(ctx, tok3.ID))
	got3, err := repo.GetByID(ctx, tok3.ID)
	require.NoError(t, err)
	require.NotNil(t, got3.LastUsedAt)
	assert.True(t, got3.LastUsedAt.After(stale.Add(time.Minute)), "stale last_used_at should be refreshed")
}
