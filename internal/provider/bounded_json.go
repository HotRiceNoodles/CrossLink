package provider

// Bounded JSON response decoding — ONE contract for every non-stream
// upstream body in the provider layer.
//
// CONTRACT (U-35 review correction): a body is acceptable iff
//   1. its TOTAL length is ≤ max (a legal JSON prefix followed by an
//      oversized tail — whitespace or garbage — is still an oversized body
//      and MUST be rejected), and
//   2. it parses as exactly one JSON value with no trailing non-whitespace
//      data.
// json.Decoder over LimitReader satisfies NEITHER: it stops at the end of
// the first JSON value (ignoring any tail) and LimitReader(n) without a
// length check cannot distinguish "exactly n" from "n and more".
//
// Mechanism: read at most max+1 bytes (bounds memory for any upstream),
// reject when len > max, then Unmarshal — encoding/json rejects trailing
// non-whitespace after the top-level value, and any over-max tail pushes the
// read to max+1 which the length check rejects.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// decodeBoundedJSON reads resp.Body with the bounded-total-length contract
// above and unmarshals into dst.
func decodeBoundedJSON(resp *http.Response, dst any, max int64, what string) error {
	buf, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", what, err)
	}
	if int64(len(buf)) > max {
		return fmt.Errorf("%s exceeds %d byte cap (total length over limit, including any trailing data)", what, max)
	}
	if err := json.Unmarshal(buf, dst); err != nil {
		return fmt.Errorf("decode %s: %w", what, err)
	}
	return nil
}
