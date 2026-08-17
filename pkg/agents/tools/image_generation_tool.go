package tools

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type ImageGenerationTool struct {
	*agents.BaseTool
}

func NewImageGenerationTool() *ImageGenerationTool {
	return &ImageGenerationTool{
		BaseTool: &agents.BaseTool{},
	}
}

func (t *ImageGenerationTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return nil, nil
}

func (t *ImageGenerationTool) Tool(ctx context.Context) *responses.ToolUnion {
	return &responses.ToolUnion{OfImageGeneration: &responses.ImageGenerationTool{
		Size:    "1024x1024",
		Quality: "low",
	}}
}
