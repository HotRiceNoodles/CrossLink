package settings

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIMap renders the snapshot as the flat key→value map used by
// GET /system/config. Sensitive keys are omitted (the handler substitutes an
// empty string); durations are exposed as seconds (their storage unit).
func (s *Snapshot) APIMap() map[string]any {
	out := make(map[string]any, len(Registry))
	setIfNotSensitive := func(d KeyDef, v any) {
		if !d.Sensitive {
			out[d.Name] = v
		}
	}
	for _, d := range Registry {
		switch d.Name {
		case KeySMTPHost:
			setIfNotSensitive(d, s.SMTP.Host)
		case KeySMTPPort:
			setIfNotSensitive(d, s.SMTP.Port)
		case KeySMTPUsername:
			setIfNotSensitive(d, s.SMTP.Username)
		case KeySMTPPassword:
			// never returned
		case KeySMTPFrom:
			setIfNotSensitive(d, s.SMTP.From)
		case KeyGatewayBaseURL:
			setIfNotSensitive(d, s.GatewayBaseURL)
		case KeyCORSOrigins:
			setIfNotSensitive(d, s.CORSAllowed)
		case KeyRateLimitRPM:
			setIfNotSensitive(d, s.RateLimit.RPM)
		case KeyRateLimitTPM:
			setIfNotSensitive(d, s.RateLimit.TPM)
		case KeyRateLimitReserve:
			setIfNotSensitive(d, s.RateLimit.Reservation)
		case KeyRateLimitFail:
			setIfNotSensitive(d, s.RateLimit.FailClosed)
		case KeyCaptchaEnabled:
			setIfNotSensitive(d, s.Captcha.Enabled)
		case KeyCaptchaProvider:
			setIfNotSensitive(d, s.Captcha.Provider)
		case KeyCaptchaTrustDays:
			setIfNotSensitive(d, s.Captcha.TrustDays)
		case KeyCaptchaIPMask:
			setIfNotSensitive(d, s.Captcha.TrustIPMask)
		case KeyCaptchaFailOpen:
			setIfNotSensitive(d, s.Captcha.RedisFailOpen)
		case KeySliderTolerance:
			setIfNotSensitive(d, s.Captcha.Slider.TolerancePx)
		case KeySliderMinPoints:
			setIfNotSensitive(d, s.Captcha.Slider.MinPoints)
		case KeySliderBGWidth:
			setIfNotSensitive(d, s.Captcha.Slider.BGWidth)
		case KeySliderBGHeight:
			setIfNotSensitive(d, s.Captcha.Slider.BGHeight)
		case KeySliderPieceSize:
			setIfNotSensitive(d, s.Captcha.Slider.PieceSize)
		case KeyDataLensEnabled:
			setIfNotSensitive(d, s.DataLens.Enabled)
		case KeyDataLensInterval:
			setIfNotSensitive(d, s.DataLens.Agg.Interval)
		case KeyDataLensLookback:
			setIfNotSensitive(d, s.DataLens.Agg.Lookback)
		case KeyDataLensBackfill:
			setIfNotSensitive(d, s.DataLens.Agg.BackfillDays)
		case KeyDataLensRawLogs:
			setIfNotSensitive(d, s.DataLens.Retention.RawLogsDays)
		case KeyDataLensHourly:
			setIfNotSensitive(d, s.DataLens.Retention.HourlyDays)
		case KeyDataLensDaily:
			setIfNotSensitive(d, s.DataLens.Retention.DailyDays)
		case KeyDataLensFromName:
			setIfNotSensitive(d, s.DataLens.FromName)
		case KeyDataLensFromAddr:
			setIfNotSensitive(d, s.DataLens.FromAddr)
		case KeyMCPEnabled:
			setIfNotSensitive(d, s.MCP.Enabled)
		case KeyMCPMaxServers:
			setIfNotSensitive(d, s.MCP.MaxServers)
		case KeyMCPToolCacheTTL:
			setIfNotSensitive(d, int(s.MCP.ToolCacheTTL/time.Second))
		case KeyMCPRequestTTL:
			setIfNotSensitive(d, int(s.MCP.RequestTimeout/time.Second))
		case KeyMCPHealthCheck:
			setIfNotSensitive(d, int(s.MCP.HealthCheckInterval/time.Second))
		case KeyMCPMaxIdleConns:
			setIfNotSensitive(d, s.MCP.HTTPMaxIdleConns)
		case KeyMCPRLEnabled:
			setIfNotSensitive(d, s.MCP.RateLimitEnabled)
		case KeyMCPRLDefaultRPM:
			setIfNotSensitive(d, s.MCP.RateLimitDefaultRPM)
		case KeyMCPLogRetention:
			setIfNotSensitive(d, s.MCP.LogRetentionDays)
		case KeyGAEnabled:
			setIfNotSensitive(d, s.GuardrailAlert.Enabled)
		case KeyGAConcurrency:
			setIfNotSensitive(d, s.GuardrailAlert.Concurrency)
		case KeyGAContentPreview:
			setIfNotSensitive(d, s.GuardrailAlert.ContentPreview)
		case KeyGAPreviewLen:
			setIfNotSensitive(d, s.GuardrailAlert.ContentPreviewLen)
		case KeyIPBCooldown:
			setIfNotSensitive(d, s.IPBinding.NotifyCooldownSeconds)
		}
	}
	return out
}

// APIMeta renders per-key metadata for GET /system/config. SetMap supplies
// the "configured" bit for sensitive keys (value itself never leaves).
type APIMeta struct {
	Section         string `json:"section"`
	Source          string `json:"source"`
	RestartRequired bool   `json:"restart_required"`
	Sensitive       bool   `json:"sensitive"`
	Type            string `json:"type"`
	Set             bool   `json:"set,omitempty"`
}

func (s *Snapshot) MetaMap() map[string]APIMeta {
	out := make(map[string]APIMeta, len(Registry))
	for _, d := range Registry {
		m := APIMeta{
			Section:         d.Section,
			Source:          s.Source(d.Name),
			RestartRequired: d.RestartRequired,
			Sensitive:       d.Sensitive,
			Type:            d.Type,
		}
		if d.Sensitive {
			m.Set = s.SensitiveSet[d.Name]
		}
		out[d.Name] = m
	}
	return out
}

// ParseAndValidate validates and stringifies one PUT payload value for
// storage. Returns the storage string. Invalid input yields an error with
// the key name in the message. Callers treat JSON null before calling this
// (null = delete row) and apply their own cross-field guards (e.g. the CORS
// self-lockout check, gateway_base_url internal-host check).
func ParseAndValidate(key string, raw json.RawMessage) (string, error) {
	d, ok := RegistryMap[key]
	if !ok {
		return "", fmt.Errorf("unknown config key %q", key)
	}
	switch d.Type {
	case TypeString:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("%s: expected string", key)
		}
		if key == KeyDataLensInterval || key == KeyDataLensLookback {
			if _, err := time.ParseDuration(v); err != nil {
				return "", fmt.Errorf("%s: invalid duration %q", key, v)
			}
		}
		return v, nil
	case TypeInt:
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("%s: expected integer", key)
		}
		if err := checkRange(key, d, float64(v)); err != nil {
			return "", err
		}
		return strconv.FormatInt(v, 10), nil
	case TypeFloat:
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("%s: expected number", key)
		}
		if err := checkRange(key, d, v); err != nil {
			return "", err
		}
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case TypeBool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("%s: expected boolean", key)
		}
		return strconv.FormatBool(v), nil
	case TypeStringSlice:
		var v []string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("%s: expected array of strings", key)
		}
		if key == KeyCORSOrigins {
			if len(v) == 0 {
				return "", fmt.Errorf("%s: must contain at least one origin (use null to reset to defaults)", key)
			}
			for _, o := range v {
				if !validOrigin(o) {
					return "", fmt.Errorf("%s: invalid origin %q (expected scheme://host[:port])", key, o)
				}
			}
		}
		data, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(data), nil
	case TypeDurationSeconds:
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("%s: expected duration in seconds", key)
		}
		if v < 0 {
			return "", fmt.Errorf("%s: must be >= 0", key)
		}
		return strconv.FormatInt(v, 10), nil
	}
	return "", fmt.Errorf("%s: unsupported type", key)
}

func checkRange(key string, d KeyDef, v float64) error {
	if !d.hasRange {
		return nil
	}
	if d.Max > d.Min && (v < d.Min || v > d.Max) {
		return fmt.Errorf("%s: must be between %v and %v", key, d.Min, d.Max)
	}
	if v < d.Min {
		return fmt.Errorf("%s: must be >= %v", key, d.Min)
	}
	return nil
}

// validOrigin checks scheme://host[:port] with no path/query — the shape CORS
// allowlists need.
func validOrigin(o string) bool {
	o = strings.TrimSpace(o)
	if o == "" || len(o) > 512 {
		return false
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	s := strings.ToLower(u.Scheme)
	if s != "http" && s != "https" {
		return false
	}
	return u.Host != "" && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}
