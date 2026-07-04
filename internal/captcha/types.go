package captcha

// Point is a single sampled coordinate during a slider drag.
// X, Y are pixel offsets from the puzzle origin; TMs is milliseconds
// elapsed since drag start. Tags match the Vue CaptchaSlider wire shape
// (lowercase keys; "t" for the timestamp). Without json:"t" the timestamp
// decodes as 0 and every trajectory is rejected as duration_too_short.
type Point struct {
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	TMs int64   `json:"t"`
}

// Verdict is the result of evaluating a slider drag.
type Verdict struct {
	Pass    bool
	Score   float64
	Reasons []string
}
