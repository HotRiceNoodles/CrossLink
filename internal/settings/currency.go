package settings

import (
	"sync/atomic"

	"github.com/crosslink/internal/config"
)

// The deployment's settlement currency (single-currency design): every model
// price, usage cost, budget limit and statistic is denominated in exactly one
// currency, chosen at first install and immutable afterwards. The value lives
// in system_settings (key billing_currency, WriteOnce); the Provider syncs
// this process-global on every snapshot load, so all writers — usage logging,
// budget settle, alert emails, exports — read one consistent currency without
// threading the Provider through every call site.
//
// WHICH currency is the operator's choice: any well-formed ISO 4217-style
// code (config.ValidCurrencyCode) is accepted. There is no list of supported
// currencies here on purpose.

// CurrencyCNY is the fallback in effect before a deployment has chosen its
// currency (see PlatformCurrency). A default, not a limit.
const CurrencyCNY = "CNY"

// platformCurrency holds the current settlement currency ("" = not yet
// selected, only possible pre-onboarding on a fresh install).
var platformCurrency atomic.Value

// SetPlatformCurrency updates the process-global currency. Called by the
// Provider on snapshot load and right after the choice is first written;
// tests may call it directly. Callers pass an already validated code — this
// value is stamped on every billing row.
func SetPlatformCurrency(c string) {
	platformCurrency.Store(c)
}

// PlatformCurrency returns the deployment's settlement currency. Never empty:
// a fresh install that has not run onboarding yet falls back to CNY — in
// practice unreachable for billing traffic (the wizard forces a selection
// before any provider/model exists), and usage rows must never carry "".
func PlatformCurrency() string {
	if c, ok := platformCurrency.Load().(string); ok && c != "" {
		return c
	}
	return CurrencyCNY
}

// BillingCurrencySet reports whether raw is a usable settlement currency
// code — i.e. whether the deployment's write-once choice has been made.
func BillingCurrencySet(raw string) bool {
	return config.ValidCurrencyCode(config.NormalizeCurrencyCode(raw))
}
