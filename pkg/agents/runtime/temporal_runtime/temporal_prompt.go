package temporal_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"go.temporal.io/sdk/workflow"
)

type TemporalPrompt struct {
	wrappedPrompt agents.SystemPromptProvider
}

func NewTemporalPrompt(wrappedPrompt agents.SystemPromptProvider) *TemporalPrompt {
	return &TemporalPrompt{
		wrappedPrompt: wrappedPrompt,
	}
}

func (p *TemporalPrompt) GetPrompt(ctx context.Context, deps *agents.Dependencies) (string, error) {
	return p.wrappedPrompt.GetPrompt(ctx, deps)
}

type TemporalPromptProxy struct {
	workflowCtx workflow.Context
	prefix      string
}

func NewTemporalPromptProxy(workflowCtx workflow.Context, prefix string) agents.SystemPromptProvider {
	return &TemporalPromptProxy{
		workflowCtx: workflowCtx,
		prefix:      prefix,
	}
}

func (p *TemporalPromptProxy) GetPrompt(ctx context.Context, deps *agents.Dependencies) (string, error) {
	var prompt string
	err := workflow.ExecuteActivity(p.workflowCtx, p.prefix+"_GetPromptActivity", deps).Get(p.workflowCtx, &prompt)
	if err != nil {
		return "", err
	}

	return prompt, nil
}
