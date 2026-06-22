package router

import (
	"context"
	"errors"
)

// ErrProRequired is returned by an alias resolver when an alias is used on a
// deployment whose license tier is below Pro. Handlers map this to HTTP 403.
// Community ships no alias implementation, so it is never produced here; the
// commercial overlay's alias resolver returns it.
var ErrProRequired = errors.New("alias requires a Pro or Enterprise license")

// AliasMeta is the metadata for a resolved alias, used by handlers to annotate
// responses and enforce modality.
type AliasMeta struct {
	Name     string
	Modality string
}

// AliasResolver expands virtual model aliases. The Community build passes nil
// (no alias support); the commercial overlay injects an implementation that
// maps capability aliases to their member models.
type AliasResolver interface {
	// ResolveAlias expands an alias name. Returns isAlias=false when name is not
	// a registered alias (caller proceeds with normal resolve).
	ResolveAlias(ctx context.Context, name string, orgID int64) (routes []*RouteResult, err error, isAlias bool)
	// AliasMeta returns metadata for an alias (for response headers + modality
	// guards). Returns ok=false when name is not an alias.
	AliasMeta(ctx context.Context, name string, orgID int64) (meta AliasMeta, ok bool)
}

// AliasMetaLookup returns alias metadata for a model name, delegating to the
// injected AliasResolver. Returns (zero, false) when no resolver is wired
// (Community) or the name is not an alias. Used by handlers.
func (r *Resolver) AliasMetaLookup(ctx context.Context, name string, orgID int64) (AliasMeta, bool) {
	if r.aliasResolver == nil {
		return AliasMeta{}, false
	}
	return r.aliasResolver.AliasMeta(ctx, name, orgID)
}
