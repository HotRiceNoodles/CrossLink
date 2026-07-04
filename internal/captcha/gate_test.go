package captcha

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestGate(t *testing.T) (*Gate, *SliderProvider, *RedisStore) {
	t.Helper()
	mr := miniredisRunT(t)
	rdb := newRedisClient(mr.Addr())
	t.Cleanup(func() { rdb.Close() })
	store := NewRedisStore(rdb, "captcha:")
	prov := NewSliderProvider(store, DefaultSliderConfig())
	g := NewGate(prov, testCaptchaCfg(24, 7*24*time.Hour), []byte("test-jwt-secret-0123456789-test-test"))
	g.now = func() time.Time { return testNow }
	return g, prov, store
}

func testCaptchaCfg(ipMask int, trustTTL time.Duration) CaptchaGateConfig {
	return CaptchaGateConfig{
		Enabled:       true,
		TrustDays:     int(trustTTL / (24 * time.Hour)),
		TrustIPMask:   ipMask,
		RedisFailOpen: true,
	}
}

var testNow = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

func TestGate_TrustCookie_RoundTrip(t *testing.T) {
	g, _, _ := newTestGate(t)
	c := g.IssueTrustCookie(42, "1.2.3.4")
	assert.True(t, g.WaivedByTrust(c, 42, "1.2.3.4"), "same user + same /24 IP should be waived")
}

func TestGate_TrustCookie_DifferentUser(t *testing.T) {
	g, _, _ := newTestGate(t)
	c := g.IssueTrustCookie(42, "1.2.3.4")
	assert.False(t, g.WaivedByTrust(c, 99, "1.2.3.4"), "cookie bound to user 42 must not waive user 99")
}

func TestGate_TrustCookie_DifferentIPSubnet(t *testing.T) {
	g, _, _ := newTestGate(t)
	c := g.IssueTrustCookie(42, "1.2.3.4")
	assert.False(t, g.WaivedByTrust(c, 42, "1.2.9.9"), "different /24 must not be waived")
}

func TestGate_TrustCookie_Tampered(t *testing.T) {
	g, _, _ := newTestGate(t)
	c := g.IssueTrustCookie(42, "1.2.3.4")
	assert.False(t, g.WaivedByTrust(c+"x", 42, "1.2.3.4"), "tampered cookie must not waive")
	assert.False(t, g.WaivedByTrust("garbage", 42, "1.2.3.4"))
}

func TestGate_TrustCookie_Expired(t *testing.T) {
	g, _, _ := newTestGate(t)
	c := g.IssueTrustCookie(42, "1.2.3.4")
	g.now = func() time.Time { return testNow.Add(8 * 24 * time.Hour) } // beyond 7-day TTL
	assert.False(t, g.WaivedByTrust(c, 42, "1.2.3.4"), "expired cookie must not waive")
}

func TestGate_TrustCookie_NoIPBinding(t *testing.T) {
	// mask = 0 → no IP binding; any IP for same user is waived
	g, _, _ := newTestGate(t)
	g.cfg.TrustIPMask = 0
	c := g.IssueTrustCookie(42, "1.2.3.4")
	assert.True(t, g.WaivedByTrust(c, 42, "9.9.9.9"))
}

func TestGate_HasValidTrust_TrueForValidCookie(t *testing.T) {
	g, _, _ := newTestGate(t)
	c := g.IssueTrustCookie(42, "1.2.3.4")
	// HasValidTrust does NOT check userID — it answers "is there a validly
	// signed, unexpired, IP-matching trust cookie for this device". The
	// userID match is enforced separately at login by WaivedByTrust.
	assert.True(t, g.HasValidTrust(c, "1.2.3.4"))
}

func TestGate_HasValidTrust_FalseForExpired(t *testing.T) {
	g, _, _ := newTestGate(t)
	c := g.IssueTrustCookie(42, "1.2.3.4")
	g.now = func() time.Time { return testNow.Add(8 * 24 * time.Hour) }
	assert.False(t, g.HasValidTrust(c, "1.2.3.4"))
}

func TestGate_HasValidTrust_FalseForWrongIPSubnet(t *testing.T) {
	g, _, _ := newTestGate(t)
	c := g.IssueTrustCookie(42, "1.2.3.4")
	assert.False(t, g.HasValidTrust(c, "1.2.9.9"))
}

func TestGate_HasValidTrust_FalseForTampered(t *testing.T) {
	g, _, _ := newTestGate(t)
	c := g.IssueTrustCookie(42, "1.2.3.4")
	assert.False(t, g.HasValidTrust(c+"x", "1.2.3.4"))
	assert.False(t, g.HasValidTrust("", "1.2.3.4"))
	assert.False(t, g.HasValidTrust("garbage", "1.2.3.4"))
}

func TestGate_Verify_FailOpenOnStoreError(t *testing.T) {
	g, prov, store := newTestGate(t)
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, "cx", StoredChallenge{GapX: 100, IP: "1.2.3.4"}, prov.cfg.TTL))

	// Simulate Redis failure by closing the client.
	_ = store.rdb.Close()

	pass, _ := g.Verify(ctx, "cx", "1.2.3.4", Answer{Points: humanPoints(100)})
	assert.True(t, pass, "with redis_fail_open=true, store error must waive captcha")
}

func TestGate_Verify_FailClosed(t *testing.T) {
	g, prov, store := newTestGate(t)
	g.cfg.RedisFailOpen = false
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, "cy", StoredChallenge{GapX: 100, IP: "1.2.3.4"}, prov.cfg.TTL))
	_ = store.rdb.Close()

	pass, _ := g.Verify(ctx, "cy", "1.2.3.4", Answer{Points: humanPoints(100)})
	assert.False(t, pass, "with redis_fail_open=false, store error must reject")
}
