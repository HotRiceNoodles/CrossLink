package guardrail

import (
	"strings"
	"testing"
)

func TestSlideBuffer(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		windowSize int
		wantLen    int // expected rune count after slide
	}{
		{"short buffer unchanged", "abc", 2048, 3},
		{"exactly half unchanged", strings.Repeat("x", 1024), 2048, 1024},
		{"over half truncates", strings.Repeat("x", 1500), 2048, 1024},
		{"empty buffer", "", 2048, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			buf.WriteString(tt.input)
			slideBuffer(&buf, tt.windowSize)
			got := []rune(buf.String())
			if len(got) != tt.wantLen {
				t.Errorf("slideBuffer() rune count = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestCallbackStreamGuardrail_BufferOps(t *testing.T) {
	// Construct directly with nil svc — these methods don't call svc.
	w := &CallbackStreamGuardrail{
		windowSize: 2048,
	}

	w.Append("hello")
	w.Append(" world")

	if w.BufferLen() != 11 {
		t.Errorf("BufferLen() = %d, want 11", w.BufferLen())
	}
	if w.BufferText() != "hello world" {
		t.Errorf("BufferText() = %q, want %q", w.BufferText(), "hello world")
	}
	if w.WindowSize() != 2048 {
		t.Errorf("WindowSize() = %d, want 2048", w.WindowSize())
	}

	w.Slide()
	// Buffer is only 11 runes, window/2 = 1024, so no truncation
	if w.BufferLen() != 11 {
		t.Errorf("Slide() should not truncate short buffer, got %d", w.BufferLen())
	}
}
