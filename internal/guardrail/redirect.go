package guardrail

import (
	"context"
	"fmt"
	"net/http"
)

// BlockInternalRedirect is an http.Client.CheckRedirect policy that refuses any
// redirect whose destination resolves to a restricted (internal/loopback/etc.)
// address, and caps the redirect chain at 10 hops.
//
// Redirects are driven by upstream responses rather than configuration, so
// blocking internal redirect targets is safe regardless of the configured base
// URL — it closes the SSRF tunnel where a provider returns 302 → 169.254.169.254
// or another internal address. Resolution failures fail closed (redirect refused).
func BlockInternalRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	host := req.URL.Hostname()
	if host == "" {
		return nil // nothing to validate here; the dialer still guards the connection
	}
	ctx, cancel := context.WithTimeout(req.Context(), ssrfLookupTimeout)
	defer cancel()
	if _, err := resolveAndCheck(ctx, host); err != nil {
		return fmt.Errorf("refused redirect to restricted host %q: %w", host, err)
	}
	return nil
}
