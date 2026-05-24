package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestActiveRequestTracker_GetMissing(t *testing.T) {
	tracker := NewActiveRequestTracker(nil)
	// nil Redis → Get returns error → 0
	assert.Equal(t, int64(0), tracker.Get(context.Background(), "unknown"))
}

func TestActiveRequestTracker_IncrNilRedis(t *testing.T) {
	tracker := NewActiveRequestTracker(nil)
	// Should not panic
	tracker.Incr(context.Background(), "test")
	assert.Equal(t, int64(0), tracker.Get(context.Background(), "test"))
}

func TestActiveRequestTracker_DecrNilRedis(t *testing.T) {
	tracker := NewActiveRequestTracker(nil)
	// Should not panic, should not go negative
	tracker.Decr(context.Background(), "test")
	assert.Equal(t, int64(0), tracker.Get(context.Background(), "test"))
}

func TestNewActiveRequestTracker(t *testing.T) {
	tracker := NewActiveRequestTracker(nil)
	assert.NotNil(t, tracker)
}
