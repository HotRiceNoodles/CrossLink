package captcha

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSliderProvider(t *testing.T) (*SliderProvider, *RedisStore) {
	t.Helper()
	mr := miniredisRunT(t)
	rdb := newRedisClient(mr.Addr())
	t.Cleanup(func() { rdb.Close() })
	store := NewRedisStore(rdb, "captcha:")
	return &SliderProvider{
		store: store,
		cfg: SliderConfig{
			BGWidth: 300, BGHeight: 150, PieceSize: 44,
			TolerancePx: 5, MinPoints: 5, TTL: 5 * time.Minute,
		},
	}, store
}

// humanPoints builds a passing trajectory that lands on targetX.
func humanPoints(targetX float64) []Point {
	n := 40
	pts := make([]Point, n)
	for i := range pts {
		frac := float64(i) / float64(n-1)
		eased := 3*frac*frac - 2*frac*frac*frac
		pts[i] = Point{X: targetX * eased, Y: math.Sin(frac*9) * 1.5, TMs: int64(1400 * frac)}
	}
	return pts
}

func TestSliderProvider_Verify_Pass(t *testing.T) {
	p, store := testSliderProvider(t)
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, "c1", StoredChallenge{GapX: 120, IP: "1.2.3.4"}, p.cfg.TTL))

	v := p.Verify(ctx, "c1", "1.2.3.4", Answer{Points: humanPoints(120), FinalX: 120})
	assert.True(t, v.Pass, "human trajectory landing on stored gapX should pass; reasons=%v", v.Reasons)
}

func TestSliderProvider_Verify_UnknownCaptcha(t *testing.T) {
	p, _ := testSliderProvider(t)
	v := p.Verify(context.Background(), "missing", "1.2.3.4", Answer{Points: humanPoints(100)})
	assert.False(t, v.Pass)
	assert.Contains(t, v.Reasons, "captcha_expired_or_unknown")
}

func TestSliderProvider_Verify_IPMismatch(t *testing.T) {
	p, store := testSliderProvider(t)
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, "c2", StoredChallenge{GapX: 100, IP: "1.2.3.4"}, p.cfg.TTL))

	v := p.Verify(ctx, "c2", "9.9.9.9", Answer{Points: humanPoints(100), FinalX: 100})
	assert.False(t, v.Pass)
	assert.Contains(t, v.Reasons, "ip_mismatch")
}

func TestSliderProvider_Verify_Oneshot(t *testing.T) {
	p, store := testSliderProvider(t)
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, "c3", StoredChallenge{GapX: 100, IP: "1.2.3.4"}, p.cfg.TTL))

	_ = p.Verify(ctx, "c3", "1.2.3.4", Answer{Points: humanPoints(100)}) // consumes
	v := p.Verify(ctx, "c3", "1.2.3.4", Answer{Points: humanPoints(100)})
	assert.False(t, v.Pass, "challenge must be one-shot")
	assert.Contains(t, v.Reasons, "captcha_expired_or_unknown")
}

func TestSliderProvider_Verify_NoTrajectory(t *testing.T) {
	p, store := testSliderProvider(t)
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, "c4", StoredChallenge{GapX: 100, IP: "1.2.3.4"}, p.cfg.TTL))

	v := p.Verify(ctx, "c4", "1.2.3.4", Answer{})
	assert.False(t, v.Pass)
	assert.Contains(t, v.Reasons, "no_trajectory")
}

func TestSliderProvider_Issue(t *testing.T) {
	p, store := testSliderProvider(t)
	ch, err := p.Issue(context.Background(), "1.2.3.4", "login")
	require.NoError(t, err)
	require.NotNil(t, ch)

	assert.NotEmpty(t, ch.CaptchaID)
	assert.Equal(t, "slider", ch.Provider)
	assert.NotEmpty(t, ch.BGImage, "bg image must be generated")
	assert.NotEmpty(t, ch.PuzzleImage, "puzzle piece must be generated")
	assert.Equal(t, 300, ch.BGWidth)
	assert.Equal(t, 150, ch.BGHeight)

	// Store must hold the expected gap X for the issued ID.
	stored, ok, err := store.Load(context.Background(), ch.CaptchaID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Greater(t, stored.GapX, 0.0)
	assert.Equal(t, "1.2.3.4", stored.IP)
	assert.Equal(t, "login", stored.Scene)
}
