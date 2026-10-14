package admin

import (
	"errors"
	"log/slog"
	"net/url"
	"strings"

	"github.com/crosslink/internal/guardrail"
)

// ensureOutboundURLAllowed is THE admission policy for admin-configured
// outbound targets (provider base_url, MCP servers, budget webhooks, gateway
// base URL): it delegates to guardrail.ValidateURLSafetyDetail, the exact
// policy the outbound dialers and the redirect guard enforce at connection
// time. An operator allowlist (gateway.internal_allow_cidrs) widens
// private-range targets only; loopback, link-local/cloud-metadata,
// unspecified and multicast addresses stay blocked for every configuration.
//
// When a URL passes BECAUSE of the allowlist, the decision's own resolution
// result (actual IP + matching CIDR — no second DNS lookup) is audited once
// at Info with a fixed category, so domain-named private targets are tracked
// as reliably as literal-IP ones. Refusals never emit an admission record.
func ensureOutboundURLAllowed(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	hit, err := guardrail.ValidateURLSafetyDetail(rawURL)
	if err != nil {
		return err
	}
	if hit != nil {
		slog.Info("ssrf allowlist: outbound URL admitted", "ip", hit.IP, "cidr", hit.CIDR)
	}
	return nil
}

// validateOutboundURL is the single admission step for every admin entry
// point that accepts an outbound URL: it trims surrounding whitespace,
// checks the http/https scheme, applies the shared SSRF policy, and returns
// the trimmed URL to store and dial with. On rejection the second return
// value carries a truthful, actionable reason — a malformed URL is reported
// as malformed, an unresolvable host as unresolvable, and only a genuinely
// restricted target as an internal address. Empty input is admitted as
// empty (entries keep their own optionality rules).
func validateOutboundURL(field, rawURL string) (string, string) {
	if rawURL == "" {
		return "", ""
	}
	trimmed := strings.TrimSpace(rawURL)
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", field + " is not a valid URL: " + urlParseReason(err) + " (check for stray spaces, full-width punctuation or port typos)"
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return "", field + " must start with http:// or https://"
	}
	if derr := ensureOutboundURLAllowed(trimmed); derr != nil {
		return "", outboundURLDeniedMsg(field, derr)
	}
	return trimmed, ""
}

// outboundURLDeniedMsg turns a guardrail admission failure into a stable
// operator-facing message. The classification comes from the policy layer;
// only the kind and the host/IP the operator themselves typed ever appear —
// no URL credentials, query or body content.
func outboundURLDeniedMsg(field string, err error) string {
	kind, host, ok := guardrail.OutboundErrorInfo(err)
	switch {
	case !ok:
		return field + " was rejected by the outbound URL policy"
	case kind == "restricted":
		if host == "" {
			host = "the target"
		}
		return field + " must not point to an internal address: " + host + " is not in the internal allowlist (gateway.internal_allow_cidrs)"
	case kind == "unresolved":
		if host == "" {
			host = "the host"
		}
		return field + " host could not be resolved: " + host
	default:
		return field + " is not a valid URL (check for stray spaces, full-width punctuation or port typos)"
	}
}

// urlParseReason extracts the url.Error reason without echoing the raw URL
// back (the raw string may carry credentials the admin pasted in).
func urlParseReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func sanitizeURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.User != nil {
		u.User = url.UserPassword("***", "***")
	}
	if u.Path != "" && u.Path != "/" {
		u.Path = "/***"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
