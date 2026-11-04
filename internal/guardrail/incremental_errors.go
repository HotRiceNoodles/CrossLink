package guardrail

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

func streamErrorReason(err error, policy bool) string {
	if policy {
		return "policy_match"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "limit exceeded"), strings.Contains(text, "exceeds inspection limit"):
		return "capacity_exceeded"
	case strings.Contains(text, "engine unavailable"):
		return "engine_unavailable"
	case strings.Contains(text, "invalid SSE JSON"), strings.Contains(text, "invalid trailing SSE"), strings.Contains(text, "invalid tool arguments"):
		return "parse_error"
	default:
		return "audit_error"
	}
}

func streamFailureFrame(protocol, reason string) string {
	kind, message := "guardrail_error", "stream inspection failed"
	if reason == "policy_match" {
		kind = "guardrail_blocked"
		message = "blocked by guardrail"
	}
	failure := map[string]any{"type": kind, "message": message, "reason_code": reason}
	switch protocol {
	case "anthropic":
		raw, _ := json.Marshal(map[string]any{"type": "error", "error": failure})
		return "event: error\ndata: " + string(raw) + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	case "responses":
		failure["type"] = "error"
		failure["code"] = kind
		raw, _ := json.Marshal(failure)
		return "event: error\ndata: " + string(raw) + "\n\n"
	default:
		raw, _ := json.Marshal(map[string]any{"error": failure})
		return "data: " + string(raw) + "\n\ndata: [DONE]\n\n"
	}
}
