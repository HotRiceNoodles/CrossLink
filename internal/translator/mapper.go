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
	"stop":           "end_turn",
	"length":         "max_tokens",
	"content_filter": "end_turn",
	"tool_calls":     "tool_use",
}

func finishReasonToStopReason(reason string) string {
	if v, ok := finishReasonMap[reason]; ok {
		return v
	}
	return "end_turn"
}
