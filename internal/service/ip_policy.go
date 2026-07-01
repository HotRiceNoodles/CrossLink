package service

import "github.com/crosslink/internal/model"

// IPPolicy decides whether a validated API key may be used from a given
// client IP. Community ships NoopPolicy (always allow); the commercial
// overlay injects a CIDR-aware implementation.
//
// The interface lives in the service package (not middleware) because the
// commercial overlay's CIDR-aware implementation naturally belongs in
// service (it depends on Redis + email-sending service code), and service
// must not import middleware (middleware already imports service).
type IPPolicy interface {
	// Check returns nil if the clientIP is permitted for this key.
	// Returns a non-nil error to deny the request (mapped to 403).
	// clientIP may be empty (malformed request); implementations MUST treat
	// empty as deny when the key has a non-empty AllowedIPs.
	// lang is the request locale (e.g. "zh-CN"/"en") — advisory, used only by
	// implementations that emit localized alerts on denial; enforcement ignores it.
	Check(key *model.APIKey, clientIP, lang string) error
}

// NoopPolicy is the Community default: always allows. The AllowedIPs field,
// if populated, is ignored in Community builds.
type NoopPolicy struct{}

func (NoopPolicy) Check(*model.APIKey, string, string) error { return nil }
