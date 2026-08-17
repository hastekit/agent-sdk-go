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
