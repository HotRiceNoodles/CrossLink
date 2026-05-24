package provider

import (
	"time"

	"github.com/crosslink/internal/model"
)

// NewFromModel creates a Provider using the adapter registry.
// Kept for backward compatibility; delegates to CreateProvider.
func NewFromModel(p *model.Provider, timeout time.Duration) (Provider, error) {
	return CreateProvider(p, timeout)
}
