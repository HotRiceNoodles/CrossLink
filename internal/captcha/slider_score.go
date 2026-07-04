package captcha

import "math"

// yJitterThreshold is the minimum Y-axis standard deviation (in pixels)
// for a trajectory to look human. A perfectly horizontal drag is bot-like.
const yJitterThreshold = 0.5

// velocityCVThreshold: a human drag accelerates then decelerates, so per-segment
// speeds vary. Below this coefficient of variation (stddev/mean), the drag is
// constant-velocity — bot-like.
const velocityCVThreshold = 0.15

// maxReversals: a real human drag has at most a couple of X-direction reversals
// (overshoot then correction). A zigzagging trajectory with many reversals is
// bot-like. Without this rule, a zigzag bot bypasses velocity CV because its
// signed speeds cancel out to a near-zero mean.
const maxReversals = 3

// ScoreSliderTrajectory evaluates whether a drag trajectory is human-like
// AND lands within tolerancePx of targetX. minPoints enforces sampling
// density.
func ScoreSliderTrajectory(points []Point, targetX, tolerancePx float64, minPoints int) Verdict {
	v := Verdict{Pass: true, Score: 1}

	if len(points) < minPoints {
		v.Pass = false
		v.Reasons = append(v.Reasons, "too_few_samples")
		return v
	}

	// Duration gate: human drags take 600ms..5s.
	duration := points[len(points)-1].TMs - points[0].TMs
	if duration < 600 {
		v.Pass = false
		v.Reasons = append(v.Reasons, "duration_too_short")
		return v
	}
	if duration > 5000 {
		v.Pass = false
		v.Reasons = append(v.Reasons, "duration_too_long")
		return v
	}

	// Y-axis jitter: a real human hand wobbles vertically. Perfectly flat = bot.
	if yStdDev(points) < yJitterThreshold {
		v.Pass = false
		v.Reasons = append(v.Reasons, "no_y_jitter")
		return v
	}

	// Velocity profile: human drags accelerate then decelerate (varied speeds).
	// Constant velocity across all segments is bot-like.
	cv := speedVariation(points)
	if cv >= 0 && cv < velocityCVThreshold {
		v.Pass = false
		v.Reasons = append(v.Reasons, "constant_velocity")
		return v
	}

	// Direction reversals: a real human overshoots at most once or twice.
	// Many reversals = zigzag bot (which otherwise bypasses velocity CV
	// because signed speeds cancel to a near-zero mean).
	if reversals(points) > maxReversals {
		v.Pass = false
		v.Reasons = append(v.Reasons, "too_many_reversals")
		return v
	}

	// Geometric landing: the final X must be within tolerance of the gap.
	finalX := points[len(points)-1].X
	if math.Abs(finalX-targetX) > tolerancePx {
		v.Pass = false
		v.Reasons = append(v.Reasons, "miss_target")
		return v
	}

	return v
}

// reversals counts the number of times the X direction changes sign between
// consecutive non-zero-movement segments.
func reversals(points []Point) int {
	if len(points) < 2 {
		return 0
	}
	var count int
	var prevSign int // -1, 0, +1
	for i := 1; i < len(points); i++ {
		dx := points[i].X - points[i-1].X
		sign := 0
		switch {
		case dx > 0:
			sign = 1
		case dx < 0:
			sign = -1
		}
		if sign != 0 && prevSign != 0 && sign != prevSign {
			count++
		}
		if sign != 0 {
			prevSign = sign
		}
	}
	return count
}

// speedVariation returns the coefficient of variation (stddev/mean) of
// per-segment x-speeds. Returns -1 if speed cannot be computed.
func speedVariation(points []Point) float64 {
	speeds := make([]float64, 0, len(points)-1)
	for i := 1; i < len(points); i++ {
		dt := points[i].TMs - points[i-1].TMs
		if dt <= 0 {
			continue
		}
		speeds = append(speeds, (points[i].X-points[i-1].X)/float64(dt))
	}
	if len(speeds) < 2 {
		return -1
	}
	var sum float64
	for _, s := range speeds {
		sum += s
	}
	mean := sum / float64(len(speeds))
	if mean == 0 {
		return -1
	}
	var sq float64
	for _, s := range speeds {
		d := s - mean
		sq += d * d
	}
	stddev := math.Sqrt(sq / float64(len(speeds)))
	return stddev / mean
}

// yStdDev returns the population standard deviation of the Y coordinates.
func yStdDev(points []Point) float64 {
	if len(points) == 0 {
		return 0
	}
	var sum float64
	for _, p := range points {
		sum += p.Y
	}
	mean := sum / float64(len(points))
	var sq float64
	for _, p := range points {
		d := p.Y - mean
		sq += d * d
	}
	return math.Sqrt(sq / float64(len(points)))
}
