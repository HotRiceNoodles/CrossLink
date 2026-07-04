package app

import (
	"log/slog"
	"time"

	"github.com/crosslink/internal/captcha"
	"github.com/crosslink/internal/config"
	"github.com/redis/go-redis/v9"
)

// buildCaptchaGate constructs the login captcha gate from config. Cloud
// providers (turnstile / tencent / aliyun) live in the commercial overlay; in
// Community they fall back to the self-hosted slider so the gate always works.
func buildCaptchaGate(cfg config.CaptchaConfig, rdb *redis.Client, jwtSecret []byte) *captcha.Gate {
	if !cfg.Enabled {
		return captcha.NewGate(nil, captcha.CaptchaGateConfig{}, jwtSecret)
	}

	var provider captcha.Provider
	switch cfg.Provider {
	case "turnstile", "tencent", "aliyun":
		slog.Warn("captcha provider not available in Community edition, falling back to slider",
			"provider", cfg.Provider)
		fallthrough
	default:
		store := captcha.NewRedisStore(rdb, "captcha:")
		provider = captcha.NewSliderProvider(store, captcha.SliderConfig{
			BGWidth:     orDefault(cfg.Slider.BGWidth, 300),
			BGHeight:    orDefault(cfg.Slider.BGHeight, 150),
			PieceSize:   orDefault(cfg.Slider.PieceSize, 44),
			TolerancePx: orZeroDefault(cfg.Slider.TolerancePx, 5),
			MinPoints:   orDefault(cfg.Slider.MinPoints, 5),
			TTL:         5 * time.Minute,
		})
	}

	return captcha.NewGate(provider, captcha.CaptchaGateConfig{
		Enabled:       cfg.Enabled,
		TrustDays:     cfg.TrustDays,
		TrustIPMask:   cfg.TrustIPMask,
		RedisFailOpen: cfg.RedisFailOpen,
	}, jwtSecret)
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orZeroDefault(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}
