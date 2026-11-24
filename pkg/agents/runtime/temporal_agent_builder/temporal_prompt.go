package temporal_agent_builder

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"go.temporal.io/sdk/workflow"
)

const getPromptActivity = WorkflowName + "_GetPromptActivity"

// PromptInput is the prompt activity's input: the config's instruction and
// what the loop knows of the run.
type PromptInput struct {
	Instruction  string               `json:"instruction"`
	Dependencies *agents.Dependencies `json:"dependencies"`
}

// GetPrompt renders the instruction with the Builder's prompt resolvers and
// prompt middlewares.
func (a *AgentBuilder) GetPrompt(ctx context.Context, in *PromptInput) (string, error) {
	prompt := temporal_runtime.NewTemporalPrompt(a.builder.Prompt(in.Instruction), agents.PromptMiddlewaresOf(a.builder.Middlewares())...)
	return prompt.GetPrompt(ctx, in.Dependencies)
}

// promptProxy is the workflow-side system prompt: rendered in a GetPrompt
// activity carrying the config's instruction.
type promptProxy struct {
	ctx         workflow.Context
	instruction string
}

func newPromptProxy(ctx workflow.Context, instruction string) agents.SystemPromptProvider {
	return &promptProxy{ctx: ctx, instruction: instruction}
}

func (p *promptProxy) GetPrompt(_ context.Context, deps *agents.Dependencies) (string, error) {
	var prompt string
	err := workflow.ExecuteActivity(p.ctx, getPromptActivity, &PromptInput{Instruction: p.instruction, Dependencies: deps}).Get(p.ctx, &prompt)
	return prompt, err
}
