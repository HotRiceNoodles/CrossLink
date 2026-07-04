package captcha

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return &RedisStore{rdb: rdb, keyPrefix: "captcha:"}, mr
}

func TestRedisStore_SaveLoadDelete(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	in := StoredChallenge{GapX: 87.5, IP: "1.2.3.4", Scene: "login"}
	require.NoError(t, store.Save(ctx, "abc", in, 5*time.Minute))

	got, ok, err := store.Load(ctx, "abc")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, in, got)

	require.NoError(t, store.Delete(ctx, "abc"))
	_, ok2, err := store.Load(ctx, "abc")
	require.NoError(t, err)
	assert.False(t, ok2, "deleted entry must not load")
}

func TestRedisStore_LoadMissing(t *testing.T) {
	store, _ := newTestStore(t)
	got, ok, err := store.Load(context.Background(), "nope")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, StoredChallenge{}, got)
}

func TestRedisStore_TTLExpires(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, "tmp", StoredChallenge{GapX: 10}, 5*time.Minute))
	mr.FastForward(6 * time.Minute) // exceed TTL

	_, ok, err := store.Load(ctx, "tmp")
	require.NoError(t, err)
	assert.False(t, ok, "entry must expire after TTL")
}
