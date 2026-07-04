package captcha

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// flatLinearTrajectory builds a bot-like drag: perfectly horizontal,
// constant velocity, lands on target. Passes points/duration gates but
// should fail behavioral checks.
func flatLinearTrajectory(n int, targetX float64, totalMs int64) []Point {
	pts := make([]Point, n)
	for i := range pts {
		frac := float64(i) / float64(n-1)
		pts[i] = Point{X: targetX * frac, Y: 0, TMs: int64(float64(totalMs) * frac)}
	}
	return pts
}

// humanTrajectory builds a realistic drag: smoothstep ease (accelerate then
// decelerate), small Y wobble, lands at targetX. Should pass all checks.
func humanTrajectory(n int, targetX float64, totalMs int64) []Point {
	pts := make([]Point, n)
	for i := range pts {
		frac := float64(i) / float64(n-1)
		// smoothstep ease: 3t² - 2t³
		eased := 3*frac*frac - 2*frac*frac*frac
		pts[i] = Point{
			X:   targetX * eased,
			Y:   math.Sin(frac*9) * 1.5, // ~1.4 wobble cycles
			TMs: int64(float64(totalMs) * frac),
		}
	}
	return pts
}

func TestScoreSliderTrajectory_MissTarget(t *testing.T) {
	// human-like but lands at 100 while target is 80 — geometric miss
	pts := humanTrajectory(40, 100, 1400)
	v := ScoreSliderTrajectory(pts, 80, 5, 5)
	assert.False(t, v.Pass, "off-target landing must not pass")
	assert.Contains(t, v.Reasons, "miss_target")
}

func TestScoreSliderTrajectory_HumanLike(t *testing.T) {
	pts := humanTrajectory(40, 100, 1400)
	v := ScoreSliderTrajectory(pts, 100, 5, 5)
	assert.True(t, v.Pass, "realistic human drag should pass; reasons=%v", v.Reasons)
	assert.Empty(t, v.Reasons)
}

func TestScoreSliderTrajectory_ConstantVelocity(t *testing.T) {
	// y has wobble (passes jitter check) but x advances at constant speed
	// — no acceleration/deceleration profile. Bot-like.
	n := 30
	pts := make([]Point, n)
	for i := range pts {
		frac := float64(i) / float64(n-1)
		pts[i] = Point{
			X:   100 * frac,
			Y:   math.Sin(frac*6) * 2, // wobble to pass jitter check
			TMs: int64(1500 * frac),
		}
	}
	v := ScoreSliderTrajectory(pts, 100, 5, 5)
	assert.False(t, v.Pass, "constant velocity is bot-like")
	assert.Contains(t, v.Reasons, "constant_velocity")
}

func TestScoreSliderTrajectory_FlatY(t *testing.T) {
	pts := flatLinearTrajectory(30, 100, 1500)
	v := ScoreSliderTrajectory(pts, 100, 5, 5)
	assert.False(t, v.Pass, "perfectly flat Y is bot-like")
	assert.Contains(t, v.Reasons, "no_y_jitter")
}

func TestScoreSliderTrajectory_DurationTooLong(t *testing.T) {
	// 10 points stretched over 6s — suspiciously slow
	pts := make([]Point, 10)
	for i := range pts {
		pts[i] = Point{X: float64(i) * 10, Y: 1, TMs: int64(i) * 600}
	}
	v := ScoreSliderTrajectory(pts, 90, 5, 5)
	assert.False(t, v.Pass, ">5s drag must not pass")
	assert.Contains(t, v.Reasons, "duration_too_long")
}

func TestScoreSliderTrajectory_DurationTooShort(t *testing.T) {
	// 10 points but crammed into 200ms — bot-fast
	pts := make([]Point, 10)
	for i := range pts {
		pts[i] = Point{X: float64(i) * 10, Y: 0, TMs: int64(i) * 20}
	}
	v := ScoreSliderTrajectory(pts, 90, 5, 5)
	assert.False(t, v.Pass, "sub-600ms drag must not pass")
	assert.Contains(t, v.Reasons, "duration_too_short")
}

func TestScoreSliderTrajectory_TooFewPoints(t *testing.T) {
	pts := []Point{
		{X: 0, Y: 0, TMs: 0},
		{X: 100, Y: 0, TMs: 50},
	}
	v := ScoreSliderTrajectory(pts, 100, 5, 5)
	assert.False(t, v.Pass, "too few samples must not pass")
	assert.Contains(t, v.Reasons, "too_few_samples")
}
