package tools

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type CodeExecutionTool struct {
	*agents.BaseTool
}

func NewCodeExecutionTool() *CodeExecutionTool {
	return &CodeExecutionTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{OfCodeExecution: &responses.CodeExecutionTool{
				Container: &responses.CodeExecutionToolContainerUnion{
					ContainerConfig: &responses.CodeExecutionToolContainerConfig{
						Type:        "auto",
						MemoryLimit: "4g",
					},
				},
			}},
		},
	}
}

func (t *CodeExecutionTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return nil, nil
}
