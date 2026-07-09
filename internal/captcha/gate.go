package captcha

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// TrustCookieName is the device-memory cookie set after a successful captcha.
const TrustCookieName = "captcha_trust"

// CaptchaGateConfig is the subset of config the Gate needs. Kept as a local
// type so the captcha package does not depend on the config package.
type CaptchaGateConfig struct {
	Enabled       bool
	TrustDays     int  // device-cookie lifetime in days; 0 = require captcha every login
	TrustIPMask   int  // CIDR bits bound into the cookie (IPv4); 0 = no IP binding
	RedisFailOpen bool // on store/Redis errors, waive captcha (true) or reject (false)
}

// Gate bundles a Provider with device-memory (trust cookie) management. It is
// the single dependency the login handler needs: it decides whether a captcha
// is required for a user, verifies submissions, and issues trust cookies on
// success.
type Gate struct {
	provider Provider
	cfg      CaptchaGateConfig
	secret   []byte
	now      func() time.Time
}

func NewGate(provider Provider, cfg CaptchaGateConfig, jwtSecret []byte) *Gate {
	return &Gate{
		provider: provider,
		cfg:      cfg,
		secret:   deriveCaptchaKey(jwtSecret),
		now:      time.Now,
	}
}

// deriveCaptchaKey derives a purpose-bound HMAC key from the JWT signing secret.
// Previously the trust-cookie HMAC reused the raw JWT secret, coupling two
// unrelated security contexts: a leak / compromise of one could forge the other.
// The derived key is isolated to "captcha-trust-cookie" and never equals the JWT
// secret, so the two cannot be confused or cross-forged.
func deriveCaptchaKey(jwtSecret []byte) []byte {
	h := hmac.New(sha256.New, jwtSecret)
	h.Write([]byte("crosslink:captcha-trust-cookie:v1"))
	return h.Sum(nil)
}

// Enabled reports whether the captcha gate is active.
func (g *Gate) Enabled() bool { return g.cfg.Enabled && g.provider != nil }

// TrustMaxAgeSeconds returns the device-cookie lifetime in seconds (0 = session cookie).
func (g *Gate) TrustMaxAgeSeconds() int {
	if g.cfg.TrustDays <= 0 {
		return 0
	}
	return g.cfg.TrustDays * 24 * 3600
}

// Issue an unsolved challenge.
func (g *Gate) Issue(ctx context.Context, ip, scene string) (*Challenge, error) {
	return g.provider.Issue(ctx, ip, scene)
}

// Verify checks a captcha submission. Returns (pass, reason). On store/infra
// errors, the result follows RedisFailOpen.
func (g *Gate) Verify(ctx context.Context, captchaID, ip string, answer Answer) (bool, string) {
	v := g.provider.Verify(ctx, captchaID, ip, answer)
	if v.Pass {
		return true, ""
	}
	for _, r := range v.Reasons {
		if r == "captcha_store_error" {
			if g.cfg.RedisFailOpen {
				return true, "captcha_waived_fail_open"
			}
			return false, r
		}
	}
	return false, strings.Join(v.Reasons, ",")
}

// IssueTrustCookie signs a device-memory cookie for (userID, ip). Returns ""
// when TrustDays == 0 (no memory). Format: base64(payload).base64(hmac).
func (g *Gate) IssueTrustCookie(userID int64, ip string) string {
	if g.cfg.TrustDays <= 0 {
		return ""
	}
	expires := g.now().Add(time.Duration(g.cfg.TrustDays) * 24 * time.Hour).Unix()
	payload := fmt.Sprintf("%d|%s|%d", userID, ipPrefix(ip, g.cfg.TrustIPMask), expires)
	mac := hmac.New(sha256.New, g.secret)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return b64(payload) + "." + b64(string(sig))
}

// WaivedByTrust reports whether the cookie validly covers (userID, ip) —
// i.e. the device is trusted FOR THIS USER. Used at login, where the userID
// is known.
func (g *Gate) WaivedByTrust(cookie string, userID int64, ip string) bool {
	uid, ok := g.verifyTrustCookie(cookie, ip)
	return ok && uid == userID
}

// HasValidTrust reports whether a validly signed, unexpired, IP-matching
// trust cookie exists for this device, WITHOUT checking the embedded userID.
// Used pre-login (e.g. by the captcha issue endpoint) to decide whether to
// show the slider at all. The userID binding is still enforced at login by
// WaivedByTrust, so a cookie issued to user A does not waive a login as
// user B (the slider re-appears via the captcha_required recovery path).
func (g *Gate) HasValidTrust(cookie string, ip string) bool {
	_, ok := g.verifyTrustCookie(cookie, ip)
	return ok
}

// verifyTrustCookie checks signature, expiry, and IP prefix. Returns the
// embedded userID and ok=true when the cookie is structurally valid for this
// device/IP. Caller decides whether the userID matches the login target.
func (g *Gate) verifyTrustCookie(cookie string, ip string) (int64, bool) {
	if cookie == "" {
		return 0, false
	}
	parts := strings.SplitN(cookie, ".", 2)
	if len(parts) != 2 {
		return 0, false
	}
	payload, err := b64Decode(parts[0])
	if err != nil {
		return 0, false
	}
	sig, err := b64Decode(parts[1])
	if err != nil {
		return 0, false
	}
	mac := hmac.New(sha256.New, g.secret)
	mac.Write([]byte(payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return 0, false
	}
	fields := strings.Split(string(payload), "|")
	if len(fields) != 3 {
		return 0, false
	}
	uid, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	expires, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || g.now().Unix() > expires {
		return 0, false
	}
	if g.cfg.TrustIPMask > 0 && fields[1] != ipPrefix(ip, g.cfg.TrustIPMask) {
		return 0, false
	}
	return uid, true
}

// ipPrefix masks an IPv4 address to maskBits (CIDR). Returns "" when masking
// is disabled, the address is IPv6, or parsing fails (caller treats "" as no
// binding).
func ipPrefix(ipStr string, maskBits int) string {
	if maskBits <= 0 {
		return ""
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.To4() == nil {
		return ipStr // IPv6 / unparseable: bind to full string rather than silently weakening
	}
	ip4 := ip.To4()
	mask := net.CIDRMask(maskBits, 32)
	for i := 0; i < 4; i++ {
		ip4[i] &= mask[i]
	}
	return ip4.String()
}

func b64(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func b64Decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
