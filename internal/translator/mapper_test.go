package translator

import (
	"testing"
)

func TestGenerateMessageID(t *testing.T) {
	id1 := generateMessageID()
	id2 := generateMessageID()

	if len(id1) < 4 {
		t.Errorf("generateMessageID() = %q, too short", id1)
	}
	if id1[:4] != "msg_" {
		t.Errorf("generateMessageID() = %q, want prefix msg_", id1)
	}
	if id1 == id2 {
		t.Error("generateMessageID() returned same value twice")
	}
}

func TestFinishReasonToStopReason(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"content_filter", "end_turn"},
		{"tool_calls", "tool_use"},
		{"unknown", "end_turn"},
		{"", "end_turn"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			got := finishReasonToStopReason(tt.reason)
			if got != tt.want {
				t.Errorf("finishReasonToStopReason(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}
