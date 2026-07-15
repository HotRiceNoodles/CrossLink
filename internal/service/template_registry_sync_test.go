package service

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestTemplateRegistrySync_CrossInstanceInvalidate: instance A updates a template
// and publishes; instance B's subscriber invalidates its cached (stale) copy, so
// its next Get reads the fresh DB value.
func TestTemplateRegistrySync_CrossInstanceInvalidate(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db := setupTemplateRegistryDB(t)
	require.NoError(t, db.Create(&model.PromptTemplate{
		Name: "summarize", SystemPrompt: "v1", TargetFormat: "auto", Status: 1,
		VariablesSchema: datatypes.JSON([]byte(`[]`)),
	}).Error)

	// Instance B: registry + sync subscriber.
	regB := NewTemplateRegistry(db)
	syncB := NewTemplateRegistrySync(rdb, regB, db)
	ctx, cancel := context.WithCancel(context.Background())
	go syncB.Start(ctx)
	defer func() { cancel(); syncB.Stop() }()
	// Give the subscriber a moment to subscribe.
	time.Sleep(80 * time.Millisecond)

	// B caches "v1".
	got, ok := regB.Get(ctx, "summarize")
	require.True(t, ok)
	require.Equal(t, "v1", got.SystemPrompt)

	// DB changes to "v2" elsewhere; instance A broadcasts reload.
	db.Model(&model.PromptTemplate{}).Where("name = ?", "summarize").Update("system_prompt", "v2")
	// Use a throwaway registry to publish as "instance A".
	syncA := NewTemplateRegistrySync(rdb, NewTemplateRegistry(db), db)
	syncA.NotifyReload("summarize")

	// Wait for B to receive + invalidate.
	require.Eventually(t, func() bool {
		got, ok := regB.Get(ctx, "summarize")
		return ok && got.SystemPrompt == "v2"
	}, time.Second, 20*time.Millisecond, "B should see v2 after cross-instance invalidate")
}

// TestTemplateRegistrySync_NotifyReloadInvalidatesLocally: the originator drops
// its own cache immediately (without waiting for its own broadcast to echo back,
// which it skips anyway).
func TestTemplateRegistrySync_NotifyReloadInvalidatesLocally(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	db := setupTemplateRegistryDB(t)
	require.NoError(t, db.Create(&model.PromptTemplate{
		Name: "translate", SystemPrompt: "orig", TargetFormat: "auto", Status: 1,
	}).Error)

	reg := NewTemplateRegistry(db)
	sync := NewTemplateRegistrySync(rdb, reg, db)
	reg.Get(context.Background(), "translate") // cache "orig"

	db.Model(&model.PromptTemplate{}).Where("name = ?", "translate").Update("system_prompt", "new")
	sync.NotifyReload("translate") // local invalidate + publish

	got, ok := reg.Get(context.Background(), "translate")
	require.True(t, ok)
	assert.Equal(t, "new", got.SystemPrompt, "originator must see fresh value after NotifyReload")
}
