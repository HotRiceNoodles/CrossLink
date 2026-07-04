package captcha

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// zigzagBot builds a bot that oscillates horizontally around an upward trend
// (many direction reversals) with Y noise, landing exactly on target within
// normal duration. A real human drag has at most 1-2 reversals (overshoot
// correction); a 6-cycle zigzag is bot-like and must be rejected.
func zigzagBot(n int, targetX float64, totalMs int64) []Point {
	pts := make([]Point, n)
	for i := range pts {
		frac := float64(i) / float64(n-1)
		x := targetX*frac + 15*math.Sin(frac*math.Pi*6)
		pts[i] = Point{
			X:   x,
			Y:   math.Sin(frac*9) * 1.5, // wobble to pass jitter check
			TMs: int64(float64(totalMs) * frac),
		}
	}
	pts[n-1].X = targetX // land exactly on target
	return pts
}

func TestScoreSliderTrajectory_ZigzagBot(t *testing.T) {
	pts := zigzagBot(40, 100, 1400)
	v := ScoreSliderTrajectory(pts, 100, 5, 5)
	assert.False(t, v.Pass, "zigzag with many direction reversals is bot-like")
	assert.Contains(t, v.Reasons, "too_many_reversals")
}
