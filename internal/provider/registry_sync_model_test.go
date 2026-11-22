package provider

import (
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

// M1: the invalidate_model action reaches the injected router callback (the
// provider package cannot import router, so the hook is a function field).
func TestRegistrySync_InvalidateModelMessage(t *testing.T) {
	t.Parallel()
	s := NewRegistrySync(nil, NewRegistry(), nil, nil)

	var got []string
	s.OnModelInvalidated = func(name string) { got = append(got, name) }

	s.handleMessage(&redis.Message{
		Channel: RegistrySyncChannel,
		Payload: `{"action":"invalidate_model","model_name":"gpt-4","instance_id":"other-node"}`,
	})

	assert.Equal(t, []string{"gpt-4"}, got, "invalidate_model must fire the callback exactly once")

	// Missing model_name is a protocol bug, not a crash — logged and skipped.
	s.handleMessage(&redis.Message{
		Channel: RegistrySyncChannel,
		Payload: `{"action":"invalidate_model","instance_id":"other-node"}`,
	})
	assert.Len(t, got, 1)
}

// M1/F-4: the remove action fires the provider-applied hook — the three-way
// flush seam on the receiving node. (The reload path exercises the same hook
// through reloadProvider; it needs a real DB handle and is covered by the
// wiring-level behavior.)
func TestRegistrySync_ProviderAppliedHook(t *testing.T) {
	t.Parallel()
	s := NewRegistrySync(nil, NewRegistry(), nil, nil)

	var applied []string
	s.OnProviderApplied = func(name string) { applied = append(applied, name) }

	s.handleMessage(&redis.Message{
		Channel: RegistrySyncChannel,
		Payload: `{"action":"remove","provider_name":"gone","instance_id":"other-node"}`,
	})

	assert.Equal(t, []string{"gone"}, applied)
}
