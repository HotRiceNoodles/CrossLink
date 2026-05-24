package httputil

import (
	"fmt"
	"io"
	"strings"
)

type SSEEvent struct {
	Event string
	Data  string
}

func WriteSSE(w io.Writer, event SSEEvent) error {
	if event.Event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event.Event); err != nil {
			return fmt.Errorf("write event type: %w", err)
		}
	}
	// Split data by newlines for SSE spec compliance
	lines := strings.Split(event.Data, "\n")
	for _, line := range lines {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return fmt.Errorf("write event data: %w", err)
		}
	}
	if _, err := fmt.Fprint(w, "\n"); err != nil {
		return fmt.Errorf("write event separator: %w", err)
	}
	return nil
}
