package token

import "unicode"

// Estimate returns a rough token count.
// Uses ~4 chars/token for ASCII text and ~1.5 chars/token for CJK text,
// which better approximates tiktoken behavior for mixed-language content.
func Estimate(text string) int {
	runes := []rune(text)
	cjk := 0
	ascii := 0
	for _, r := range runes {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hiragana, r) {
			cjk++
		} else {
			ascii++
		}
	}
	// CJK: ~1.5 chars per token, ASCII: ~4 chars per token
	// Use float then round to avoid integer truncation (e.g. 1*2/3 = 0)
	return int(float64(cjk)/1.5+0.5) + ascii/4
}

// EstimateZeroAlloc returns the same estimate as Estimate without building a
// []rune copy — for large request payloads on the analysis path.
func EstimateZeroAlloc(text string) int {
	cjk, ascii := 0, 0
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hiragana, r) {
			cjk++
		} else {
			ascii++
		}
	}
	return int(float64(cjk)/1.5+0.5) + ascii/4
}
