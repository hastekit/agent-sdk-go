package gateway

import (
	"context"
	"maps"
)

type gatewayContextKey struct{}

type providerConfigKeyContextKey struct{}

// WithProviderConfigKey returns a copy of ctx carrying the key used to resolve
// provider configuration (a virtual key or a direct provider API key). The LLM
// client reads it via ProviderConfigKeyFromContext when picking an API key, so
// callers can scope a run to a specific credential by injecting it here before
// invoking the agent.
//
// Under the durable runtimes the plain context.Context does not survive the
// workflow/activity boundary, so each runtime re-establishes this value on the
// far side (Temporal via a ContextPropagator, Restate via the workflow input).
func WithProviderConfigKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, providerConfigKeyContextKey{}, key)
}

// ProviderConfigKeyFromContext returns the provider config key set by
// WithProviderConfigKey, or "" if none was set.
func ProviderConfigKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(providerConfigKeyContextKey{}).(string)
	return key
}

func AddContext(ctx context.Context, values map[string]string) context.Context {
	// Check if the context already has the key
	gatewayContext, ok := ctx.Value(gatewayContextKey{}).(map[string]string)
	if ok {
		maps.Copy(gatewayContext, values)
		return context.WithValue(ctx, gatewayContextKey{}, gatewayContext)
	}

	newValues := make(map[string]string, len(values))
	maps.Copy(newValues, values)

	return context.WithValue(ctx, gatewayContextKey{}, values)
}

func GetContext(ctx context.Context) map[string]string {
	gatewayContextAny := ctx.Value(gatewayContextKey{})
	gatewayContext, ok := gatewayContextAny.(map[string]string)
	if ok {
		return gatewayContext
	}
	return map[string]string{}
}
