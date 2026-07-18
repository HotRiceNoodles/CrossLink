package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/crosslink/internal/domain"
)

// Fixture is one recorded (request → response) pair for VCR-style mock playback.
type Fixture struct {
	ID           int64  `gorm:"primaryKey" json:"id"`
	ProviderName string `gorm:"size:64;not null;index" json:"provider_name"`
	Model        string `gorm:"size:128;not null" json:"model"`
	RequestHash  string `gorm:"size:64;not null" json:"request_hash"`
	ResponseBody []byte `gorm:"type:jsonb" json:"response_body,omitempty"`
	StreamChunks []byte `gorm:"type:jsonb" json:"stream_chunks,omitempty"`
	IsStream     bool   `gorm:"not null;default:false" json:"is_stream"`
}

func (Fixture) TableName() string { return "mock_fixtures" }

// FixtureStore persists and looks up recorded fixtures.
type FixtureStore interface {
	Save(ctx context.Context, f *Fixture) error
	Lookup(ctx context.Context, model, hash string) (*Fixture, bool, error)
}

// RequestHash computes a deterministic SHA-256 of model + messages (role:content).
// Volatile fields (temperature, max_tokens, stream) are intentionally excluded so
// that the same prompt hits the same fixture regardless of sampling parameters.
func RequestHash(req *domain.OpenAIRequest) string {
	var b strings.Builder
	b.WriteString(req.Model)
	for _, m := range req.Messages {
		b.WriteString("|")
		b.WriteString(m.Role)
		b.WriteString(":")
		b.WriteString(domain.ContentText(m.Content))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
