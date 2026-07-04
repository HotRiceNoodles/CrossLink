package captcha

import (
	"context"
	"time"
)

// Provider is a pluggable CAPTCHA backend. The default self-hosted
// implementation is Slider; cloud providers (Turnstile / Tencent / Aliyun)
// live in the commercial overlay and implement the same interface.
type Provider interface {
	// Name returns the provider identifier, e.g. "slider", "turnstile".
	Name() string

	// Issue generates a fresh challenge for the given IP/scene and returns
	// the data the client needs to render it.
	Issue(ctx context.Context, ip, scene string) (*Challenge, error)

	// Verify checks the client's answer against the challenge identified by
	// captchaID. The challenge is consumed (one-shot) regardless of outcome.
	Verify(ctx context.Context, captchaID, ip string, answer Answer) Verdict
}

// Challenge carries the rendering data for a CAPTCHA. Slider-specific image
// fields are populated by the slider provider; cloud providers populate only
// the relevant subset (e.g. SiteKey).
type Challenge struct {
	CaptchaID   string `json:"captcha_id"`
	Provider    string `json:"provider"`
	BGImage     string `json:"bg_image,omitempty"`     // base64 PNG of background with gap hole
	PuzzleImage string `json:"puzzle_image,omitempty"` // base64 PNG of the puzzle piece
	BGWidth     int    `json:"bg_width,omitempty"`
	BGHeight    int    `json:"bg_height,omitempty"`
	GapY        int    `json:"gap_y,omitempty"` // vertical position of the gap (client fixes piece Y)
}

// Answer is the client's submission. Slider populates FinalX + Points.
type Answer struct {
	FinalX float64 `json:"final_x"`
	Points []Point `json:"points"`
}

// StoredChallenge is the server-side secret kept in the Store; the expected
// gap X is never sent to the client.
type StoredChallenge struct {
	GapX  float64
	IP    string
	Scene string
}

// Store holds pending CAPTCHA challenges keyed by captcha ID. Each entry is
// one-shot: Verify consumes it.
type Store interface {
	Save(ctx context.Context, id string, c StoredChallenge, ttl time.Duration) error
	Load(ctx context.Context, id string) (StoredChallenge, bool, error)
	Delete(ctx context.Context, id string) error
}
