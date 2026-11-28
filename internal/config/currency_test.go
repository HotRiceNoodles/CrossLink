package config

import "testing"

// The settlement currency is an operator choice, so the rule is a FORMAT
// check: any three ASCII letters. These tests pin the exact boundary — what
// is one code, what is normalized into one, and what is refused.
func TestValidCurrencyCode(t *testing.T) {
	// Any well-formed code is valid: major currencies, minor ones, and codes
	// a curated list would not know about.
	for _, code := range []string{"CNY", "USD", "EUR", "JPY", "GBP", "HKD", "TWD", "KRW", "CHF", "INR", "BRL", "AED", "VND", "XAF", "XYZ"} {
		if !ValidCurrencyCode(code) {
			t.Errorf("ValidCurrencyCode(%q) = false, want true", code)
		}
	}
	for _, code := range []string{
		"",                        // not chosen
		"C", "CN", "EURO", "USDD", // wrong length
		"cny", "Usd", " usd", // not normalized
		"US1", "U1D", "A-B", "U_D", // not letters
		"ＵＳＤ", "€UR", // not ASCII letters
	} {
		if ValidCurrencyCode(code) {
			t.Errorf("ValidCurrencyCode(%q) = true, want false", code)
		}
	}
}

// Normalization folds spellings of one code together but never invents one:
// malformed input comes back trimmed so validation can reject it.
func TestNormalizeCurrencyCode(t *testing.T) {
	cases := map[string]string{
		" eur ": "EUR",
		"jpy":   "JPY",
		"Hkd":   "HKD",
		"USD":   "USD",
		"":      "",
		"  ":    "",
		// Left for ValidCurrencyCode to reject — not coerced to a default.
		"euro": "EURO",
		"eu":   "EU",
	}
	for in, want := range cases {
		if got := NormalizeCurrencyCode(in); got != want {
			t.Errorf("NormalizeCurrencyCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// A normalized valid code round-trips: normalization followed by validation
// accepts exactly the codes a user could mean to type.
func TestNormalizeThenValidate(t *testing.T) {
	for _, in := range []string{"eur", " jpy", "gbd ", "HkD"} {
		if !ValidCurrencyCode(NormalizeCurrencyCode(in)) {
			t.Errorf("%q should normalize into a valid code", in)
		}
	}
}
