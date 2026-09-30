package captcha

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// SliderConfig tunes the slider challenge. All fields have safe defaults
// via DefaultSliderConfig().
type SliderConfig struct {
	BGWidth     int           // background canvas width
	BGHeight    int           // background canvas height
	PieceSize   int           // gap / puzzle piece side length (px)
	TolerancePx float64       // landing tolerance passed to ScoreSliderTrajectory
	MinPoints   int           // minimum trajectory samples
	TTL         time.Duration // challenge lifetime in the store
}

func DefaultSliderConfig() SliderConfig {
	return SliderConfig{
		BGWidth: 300, BGHeight: 150, PieceSize: 44,
		TolerancePx: 5, MinPoints: 5, TTL: 5 * time.Minute,
	}
}

// SliderProvider is the self-hosted default CAPTCHA: server-generated puzzle
// images + trajectory-based Verify. Works in any environment (no external
// network).
type SliderProvider struct {
	mu    sync.RWMutex
	store Store
	cfg   SliderConfig
}

func NewSliderProvider(store Store, cfg SliderConfig) *SliderProvider {
	if cfg.BGWidth == 0 {
		cfg = DefaultSliderConfig()
	}
	return &SliderProvider{store: store, cfg: cfg}
}

// ApplySliderConfig hot-swaps the puzzle geometry (runtime settings).
func (p *SliderProvider) ApplySliderConfig(cfg SliderConfig) {
	if cfg.BGWidth == 0 {
		cfg = DefaultSliderConfig()
	}
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
}

func (p *SliderProvider) Name() string { return "slider" }

func (p *SliderProvider) Issue(ctx context.Context, ip, scene string) (*Challenge, error) {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	id := newID()
	// gap kept fully inside the canvas with horizontal room to drag from 0.
	maxX := cfg.BGWidth - cfg.PieceSize - 20
	gapX := randInt(cfg.PieceSize+20, maxX)
	gapY := randInt(20, cfg.BGHeight-cfg.PieceSize-20)

	bgPNG, piecePNG := renderSlider(cfg, gapX, gapY)

	if err := p.store.Save(ctx, id, StoredChallenge{
		GapX:  float64(gapX),
		IP:    ip,
		Scene: scene,
	}, cfg.TTL); err != nil {
		return nil, fmt.Errorf("captcha slider: store issue: %w", err)
	}

	return &Challenge{
		CaptchaID:   id,
		Provider:    "slider",
		BGImage:     bgPNG,
		PuzzleImage: piecePNG,
		BGWidth:     cfg.BGWidth,
		BGHeight:    cfg.BGHeight,
		GapY:        gapY,
	}, nil
}

func (p *SliderProvider) Verify(ctx context.Context, captchaID, ip string, answer Answer) Verdict {
	stored, ok, err := p.store.Load(ctx, captchaID)
	if ok { // consume one-shot regardless of outcome
		_ = p.store.Delete(ctx, captchaID)
	}
	if err != nil {
		return Verdict{Pass: false, Reasons: []string{"captcha_store_error"}}
	}
	if !ok {
		return Verdict{Pass: false, Reasons: []string{"captcha_expired_or_unknown"}}
	}
	if ip != "" && stored.IP != "" && stored.IP != ip {
		return Verdict{Pass: false, Reasons: []string{"ip_mismatch"}}
	}
	if len(answer.Points) == 0 {
		return Verdict{Pass: false, Reasons: []string{"no_trajectory"}}
	}
	p.mu.RLock()
	tolerance, minPoints := p.cfg.TolerancePx, p.cfg.MinPoints
	p.mu.RUnlock()
	return ScoreSliderTrajectory(answer.Points, stored.GapX, tolerance, minPoints)
}

// newID returns a fresh 32-hex-char captcha ID from crypto/rand.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is catastrophic; panic keeps the contract simple.
		panic(fmt.Sprintf("captcha: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// randInt returns a uniform random int in [lo, hi]. Panics if hi < lo.
func randInt(lo, hi int) int {
	if hi < lo {
		lo, hi = hi, lo
	}
	span := uint64(hi - lo + 1)
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("captcha: crypto/rand unavailable: %v", err))
	}
	// map 8 random bytes into [0, span)
	v := uint64(b[0])<<48 | uint64(b[1])<<40 | uint64(b[2])<<32 |
		uint64(b[3])<<24 | uint64(b[4])<<16 | uint64(b[5])<<8 | uint64(b[6])
	return lo + int(v%span)
}
