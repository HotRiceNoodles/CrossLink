package upstream

import "context"

// ── Fallback marker ──

type fallbackKey struct{}

func WithFallback(ctx context.Context, v bool) context.Context {
	return context.WithValue(ctx, fallbackKey{}, v)
}

func IsFallbackFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(fallbackKey{}).(bool)
	return v
}

// ── Provider metadata ──

type providerNameKey struct{}
type providerModelKey struct{}
type providerBaseURLKey struct{}

func WithProviderName(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, providerNameKey{}, v)
}

func ProviderNameFromContext(ctx context.Context) string {
	v, _ := ctx.Value(providerNameKey{}).(string)
	return v
}

func WithProviderModel(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, providerModelKey{}, v)
}

func ProviderModelFromContext(ctx context.Context) string {
	v, _ := ctx.Value(providerModelKey{}).(string)
	return v
}

func WithProviderBaseURL(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, providerBaseURLKey{}, v)
}

func ProviderBaseURLFromContext(ctx context.Context) string {
	v, _ := ctx.Value(providerBaseURLKey{}).(string)
	return v
}
