package gateway

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
)

// Tracing for these requests is handled by TracingMiddleware, not inline.

func (g *LLMGateway) handleImageEditRequest(ctx context.Context, providerName llm.ProviderName, p llm.Provider, in *image_edit.Request) (*image_edit.Response, error) {
	return p.NewImageEdit(ctx, in)
}
