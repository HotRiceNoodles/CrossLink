package provider

// U-35 contract tests (review correction): bounded decoding must prove the
// TOTAL body ≤ cap — a legal JSON prefix plus an oversized tail (whitespace
// OR garbage) is an oversized body and must be rejected; exact-cap bodies are
// accepted; truncated bodies fail; read errors (client cancel) fail. The old
// json.Decoder-over-LimitReader satisfied none of this (it stops at the end
// of the first JSON value, silently accepting any tail that fits the read).

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func respWith(body string) *http.Response {
	return &http.Response{Body: io.NopCloser(strings.NewReader(body))}
}

func TestDecodeBoundedJSON_Contract(t *testing.T) {
	const max = 64
	small := `{"ok":true}` // 11 bytes

	t.Run("under cap accepts", func(t *testing.T) {
		var dst struct{ OK bool }
		if err := decodeBoundedJSON(respWith(small), &dst, max, "x"); err != nil {
			t.Fatalf("small body rejected: %v", err)
		}
		if !dst.OK {
			t.Fatal("value not decoded")
		}
	})

	t.Run("exact cap accepts", func(t *testing.T) {
		body := `{"pad":"` + strings.Repeat("a", max-len(`{"pad":""}`)) + `"}` // exactly max bytes
		if len(body) != max {
			t.Fatalf("fixture: %d bytes, want %d", len(body), max)
		}
		var dst struct{ Pad string }
		if err := decodeBoundedJSON(respWith(body), &dst, max, "x"); err != nil {
			t.Fatalf("exact-cap body rejected: %v", err)
		}
	})

	t.Run("cap plus one rejects", func(t *testing.T) {
		body := `{"pad":"` + strings.Repeat("a", max-len(`{"pad":""}`)+1) + `"}` // max+1
		var dst struct{ Pad string }
		if err := decodeBoundedJSON(respWith(body), &dst, max, "x"); err == nil {
			t.Fatal("max+1 body must be rejected")
		}
	})

	t.Run("legal JSON plus oversized whitespace tail rejects", func(t *testing.T) {
		// THE case the old Decoder silently accepted: complete JSON value,
		// then megabytes of trailing whitespace beyond the cap.
		body := small + strings.Repeat(" ", max)
		var dst struct{ OK bool }
		if err := decodeBoundedJSON(respWith(body), &dst, max, "x"); err == nil {
			t.Fatal("JSON + oversized whitespace tail must be rejected (total length over cap)")
		}
	})

	t.Run("legal JSON plus garbage tail rejects", func(t *testing.T) {
		body := small + "TRAILING-GARBAGE-BEYOND-THE-CAP-PAD-PAD-PAD-PAD-PAD!!"
		var dst struct{ OK bool }
		err := decodeBoundedJSON(respWith(body), &dst, max, "x")
		if err == nil {
			t.Fatal("trailing garbage must be rejected")
		}
	})

	t.Run("garbage tail within cap rejects at unmarshal", func(t *testing.T) {
		// Total ≤ cap but the body is two values / value+garbage: Unmarshal
		// must refuse (json.Unmarshal rejects trailing non-whitespace).
		body := small + "garbage"
		var dst struct{ OK bool }
		if err := decodeBoundedJSON(respWith(body), &dst, max, "x"); err == nil {
			t.Fatal("non-JSON trailing data within cap must be rejected")
		}
	})

	t.Run("truncated JSON rejects", func(t *testing.T) {
		body := `{"pad":"` + strings.Repeat("a", max-10) // cut mid-string, ≤ cap
		var dst struct{ Pad string }
		if err := decodeBoundedJSON(respWith(body), &dst, max, "x"); err == nil {
			t.Fatal("truncated JSON must be rejected")
		}
	})

	t.Run("read error (cancel) rejects", func(t *testing.T) {
		r := io.NopCloser(errReader2{})
		var dst struct{ OK bool }
		if err := decodeBoundedJSON(&http.Response{Body: r}, &dst, max, "x"); err == nil {
			t.Fatal("read error must surface, not masquerade as success")
		}
	})
}

type errReader2 struct{}

func (errReader2) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
