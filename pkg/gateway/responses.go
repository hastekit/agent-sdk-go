package gateway

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// Tracing for these requests is handled by TracingMiddleware, not inline.

func (g *LLMGateway) handleResponsesRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *responses.Request) (*responses.Response, error) {
	return p.NewResponses(ctx, in)
}

func (g *LLMGateway) handleStreamingResponsesRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *responses.Request) (chan *responses.ResponseChunk, error) {
	return p.NewStreamingResponses(ctx, in)
}
