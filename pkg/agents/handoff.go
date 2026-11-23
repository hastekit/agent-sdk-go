package agents

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type HandoffTarget interface {
	ExecuteWithRun(ctx context.Context, in *AgentInput, run *history.ConversationRunManager) (*AgentOutput, error)
}

type Handoff struct {
	Name        string
	Description string
	Agent       HandoffTarget `json:"-"`
}

func NewHandoff(name string, desc string, agent *Agent) *Handoff {
	return &Handoff{
		Name:        name,
		Description: desc,
		Agent:       agent,
	}
}

type HandoffTool struct {
	*BaseTool
	agent *Agent
}

func NewHandoffTool(t *responses.ToolUnion) *HandoffTool {
	return &HandoffTool{
		BaseTool: &BaseTool{
			ToolUnion: *t,
		},
	}
}

func (t *HandoffTool) Execute(ctx context.Context, params *ToolCall) (*ToolCallResponse, error) {
	return nil, nil
}
