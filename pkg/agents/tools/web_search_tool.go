package tools

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type WebSearchTool struct {
	*agents.BaseTool
}

func NewWebSearchTool() *WebSearchTool {
	return &WebSearchTool{
		BaseTool: &agents.BaseTool{},
	}
}

func (t *WebSearchTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return nil, nil
}

func (t *WebSearchTool) Tool(ctx context.Context) *responses.ToolUnion {
	return &responses.ToolUnion{OfWebSearch: &responses.WebSearchTool{}}
}
