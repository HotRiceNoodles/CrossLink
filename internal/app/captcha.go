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
//
// The slider provider is constructed even when the gate is disabled — the
// runtime settings provider can hot-enable the gate later (Gate.ApplyConfig),
// which requires a live provider. Construction is cheap and the disabled path
// never touches the store.
func buildCaptchaGate(cfg config.CaptchaConfig, rdb *redis.Client, jwtSecret []byte) *captcha.Gate {
	if cfg.Provider == "turnstile" || cfg.Provider == "tencent" || cfg.Provider == "aliyun" {
		slog.Warn("captcha provider not available in Community edition, falling back to slider",
			"provider", cfg.Provider)
	}

	gateCfg, sliderCfg := captchaConfigsFrom(cfg)
	store := captcha.NewRedisStore(rdb, "captcha:")
	provider := captcha.NewSliderProvider(store, sliderCfg)
	return captcha.NewGate(provider, gateCfg, jwtSecret)
}

// captchaConfigsFrom maps a config.CaptchaConfig onto the captcha package's
// gate + slider config shapes (with yaml defaults applied). Shared by
// buildCaptchaGate (initial) and the runtime-settings subscriber (hot swap).
func captchaConfigsFrom(cfg config.CaptchaConfig) (captcha.CaptchaGateConfig, captcha.SliderConfig) {
	return captcha.CaptchaGateConfig{
			Enabled:       cfg.Enabled,
			TrustDays:     cfg.TrustDays,
			TrustIPMask:   cfg.TrustIPMask,
			RedisFailOpen: cfg.RedisFailOpen,
		}, captcha.SliderConfig{
			BGWidth:     orDefault(cfg.Slider.BGWidth, 300),
			BGHeight:    orDefault(cfg.Slider.BGHeight, 150),
			PieceSize:   orDefault(cfg.Slider.PieceSize, 44),
			TolerancePx: orZeroDefault(cfg.Slider.TolerancePx, 5),
			MinPoints:   orDefault(cfg.Slider.MinPoints, 5),
			TTL:         5 * time.Minute,
		}
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
