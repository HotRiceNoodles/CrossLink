package translator

import (
	"crypto/rand"
	"encoding/hex"
)

func generateMessageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "msg_" + hex.EncodeToString(b)
}

var finishReasonMap = map[string]string{
	"stop":   "end_turn",
	"length": "max_tokens",
	// Phase-2 refusal fidelity: content_filter is the openai-internal pair of
	// an anthropic refusal — mapping it to end_turn made a refused answer
	// indistinguishable from a normal completion on the anthropic-inbound
	// round trip.
	"content_filter": "refusal",
	"tool_calls":     "tool_use",
}

func finishReasonToStopReason(reason string) string {
	if v, ok := finishReasonMap[reason]; ok {
		return v
	}
	return "end_turn"
}
