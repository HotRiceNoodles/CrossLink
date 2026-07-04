package captcha

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPoint_JSONDecodesFrontendShape guards the wire contract: the Vue
// CaptchaSlider emits points as {"x","y","t"} (lowercase, "t" for the ms
// timestamp). Go's case-insensitive JSON matching covers x↔X and y↔Y, but
// NOT t↔TMs — so without an explicit json:"t" tag every timestamp decodes
// as 0 and ScoreSliderTrajectory rejects all real drags with
// duration_too_short.
func TestPoint_JSONDecodesFrontendShape(t *testing.T) {
	raw := `{"x": 42.5, "y": 1.3, "t": 1234}`
	var p Point
	require.NoError(t, json.Unmarshal([]byte(raw), &p))
	assert.Equal(t, 42.5, p.X)
	assert.Equal(t, 1.3, p.Y)
	assert.Equal(t, int64(1234), p.TMs, "timestamp field must decode from frontend 't' key")
}

func TestAnswer_JSONDecodesFrontendShape(t *testing.T) {
	raw := `{"final_x": 100, "points": [{"x":0,"y":0,"t":0},{"x":100,"y":1,"t":1400}]}`
	var a Answer
	require.NoError(t, json.Unmarshal([]byte(raw), &a))
	assert.Equal(t, 100.0, a.FinalX)
	require.Len(t, a.Points, 2)
	assert.Equal(t, int64(1400), a.Points[1].TMs)
}
