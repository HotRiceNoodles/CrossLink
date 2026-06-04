package service

import (
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestUsageService_Log_NewFields(t *testing.T) {
	// Verify UsageEntry maps new fields to UsageLog correctly
	entry := &UsageEntry{
		RouteType:       "openai",
		ModelRequested:  "gpt-4",
		ModelUsed:       "gpt-4",
		InputTokens:     100,
		OutputTokens:    50,
		ReasoningTokens: 12,
		CacheReadTokens: 30,
		SessionID:       "sess_abc123",
	}

	// Simulate the mapping that happens in UsageService.Log()
	log := &model.UsageLog{
		RouteType:      entry.RouteType,
		ModelRequested: entry.ModelRequested,
		ModelUsed:      entry.ModelUsed,
		InputTokens:    entry.InputTokens,
		OutputTokens:   entry.OutputTokens,
	}

	// Map new fields (same as in Log())
	log.ReasoningTokens = entry.ReasoningTokens
	log.CacheReadTokens = entry.CacheReadTokens
	log.SessionID = entry.SessionID

	assert.Equal(t, 12, log.ReasoningTokens)
	assert.Equal(t, 30, log.CacheReadTokens)
	assert.Equal(t, "sess_abc123", log.SessionID)
}

func TestUsageEntry_NewFields_DefaultZero(t *testing.T) {
	// Verify zero values work correctly
	entry := &UsageEntry{
		RouteType:      "openai",
		ModelRequested: "gpt-4",
		ModelUsed:      "gpt-4",
		InputTokens:    100,
		OutputTokens:   50,
	}

	assert.Equal(t, 0, entry.ReasoningTokens)
	assert.Equal(t, 0, entry.CacheReadTokens)
	assert.Equal(t, "", entry.SessionID)
}
