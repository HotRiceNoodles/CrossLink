package config

import "strings"

// The settlement currency is an ISO 4217-style code: exactly three ASCII
// letters (CNY, USD, EUR, JPY, HKD, ...). The platform settles in exactly ONE
// currency, but which one is the operator's choice, so there is deliberately
// no fixed list here: a list would go stale as ISO assigns codes and would
// block a currency an operator actually needs.
//
// Validity is therefore a FORMAT check only. Guarding against a mistyped but
// well-formed code (UDS for USD) belongs to the choosing UI, which is the
// only place that can show the operator what a code means before it is locked.

// NormalizeCurrencyCode trims and upper-cases a currency code (" eur " →
// "EUR"). It does not validate: malformed input comes back trimmed so the
// caller can check it.
func NormalizeCurrencyCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// ValidCurrencyCode reports whether code is a well-formed settlement currency
// code. The input is expected to be already normalized.
func ValidCurrencyCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return false
		}
	}
	return true
}
