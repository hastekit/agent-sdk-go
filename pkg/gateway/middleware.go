package gateway

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
)

type RequestHandler func(ctx context.Context, providerName llm.ProviderName, key string, r *llm.Request) (*llm.Response, error)
type StreamingRequestHandler func(ctx context.Context, providerName llm.ProviderName, key string, r *llm.Request) (*llm.StreamingResponse, error)

type Middleware interface {
	HandleRequest(next RequestHandler) RequestHandler
	HandleStreamingRequest(next StreamingRequestHandler) StreamingRequestHandler
}

// ConfigStoreAware is an optional Middleware capability: the middleware needs
// the provider configuration, and UseMiddleware injects the store the gateway
// was built with.
//
// It exists so that middleware needing the store — fallback, which resolves a
// key for a provider the caller never named — can be constructed without one,
// at a call site that has not built a gateway yet.
type ConfigStoreAware interface {
	Middleware

	// WithConfigStore returns a copy bound to store, rather than mutating,
	// so middleware shared between gateways is never re-pointed at another
	// gateway's configuration. A store set by the caller wins; injection
	// only fills a gap.
	WithConfigStore(store ConfigStore) Middleware
}
