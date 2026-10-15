package service

import (
	"strings"
	"testing"
)

// Regression (graceful-degradation laundering): when the upstream SSE stream
// dropped mid-flight (no finish_reason, no [DONE]), OnProviderDisconnect used
// to synthesize message_delta(end_turn) + message_stop, presenting the
// truncation to /v1/messages clients as a normal completion. It must instead
// surface a structured stream_interrupted error event — never end_turn.
func TestMessagesPath_MidStreamDrop_InterruptedNotLaundered(t *testing.T) {
	payload := "data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"par"},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"tial"},"finish_reason":null}]}` + "\n\n"
	// stream ends here — upstream closed without terminal

	events := runMessagesStream(t, payload)

	if got := countEvents(events, "message_stop"); got != 1 {
		t.Fatalf("message_stop = %d, want exactly 1 (from the interrupted close-out)", got)
	}
	for _, e := range events {
		if e.Event == "message_delta" && strings.Contains(e.Data, `"stop_reason":"end_turn"`) {
			t.Fatalf("truncation laundered into end_turn: %s", e.Data)
		}
	}
	foundInterrupted := false
	for _, e := range events {
		if e.Event == "error" && strings.Contains(e.Data, "stream_interrupted") {
			foundInterrupted = true
		}
	}
	if !foundInterrupted {
		t.Fatalf("no stream_interrupted error event in %v", eventNames(events))
	}
	// Content seen before the drop must still be delivered (split across deltas).
	joined := ""
	for _, e := range events {
		joined += e.Data
	}
	if !strings.Contains(joined, "par") || !strings.Contains(joined, "tial") {
		t.Fatal("pre-drop content lost")
	}
}

func eventNames(events []StreamEvent) []string {
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.Event)
	}
	return names
}

// Clean terminal (finish_reason) without [DONE] must NOT be affected by the
// interrupted close-out — it keeps exactly one message_stop and end_turn.
func TestMessagesPath_CleanFinish_Unaffected(t *testing.T) {
	payload := "data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"

	events := runMessagesStream(t, payload)

	if got := countEvents(events, "message_stop"); got != 1 {
		t.Fatalf("message_stop = %d, want 1", got)
	}
	foundEndTurn := false
	for _, e := range events {
		if e.Event == "message_delta" && strings.Contains(e.Data, `"stop_reason":"end_turn"`) {
			foundEndTurn = true
		}
	}
	if !foundEndTurn {
		t.Fatal("clean finish lost its end_turn mapping")
	}
	for _, e := range events {
		if e.Event == "error" {
			t.Fatalf("clean stream must not carry error events: %s", e.Data)
		}
	}
}
