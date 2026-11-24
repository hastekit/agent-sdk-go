package temporal_agent_builder

import (
	"context"
	"fmt"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const subAgentCallActivity = WorkflowName + "_SubAgentCallActivity"

// SubAgentCallTimeout bounds one sub-agent call: the sub-agent's whole run,
// which the activity waits for.
const SubAgentCallTimeout = time.Hour

// SubAgentCallInput is the sub-agent activity's input: the run's snapshot, the
// agent calling, and the call, whose name is the sub-agent's tool.
type SubAgentCallInput struct {
	Snapshot *agentconfig.Snapshot `json:"snapshot"`
	Agent    string                `json:"agent"`
	Call     *agents.ToolCall      `json:"call"`
}

// SubAgentCall runs one sub-agent call: it builds the tool from the snapshot,
// as the workflow did to describe it, and runs it — the sub-agent's own run,
// which on this runtime is a workflow of its own the activity waits for.
func (a *AgentBuilder) SubAgentCall(ctx context.Context, in *SubAgentCallInput) (*agents.ToolCallResponse, error) {
	if in.Snapshot == nil || in.Call == nil {
		return nil, configError(fmt.Errorf("%w: sub-agent call without its snapshot or call", agentconfig.ErrInvalid))
	}
	for _, tool := range a.builder.SubAgentTools(in.Snapshot, in.Agent) {
		if function := tool.GetToolDescriptor().ToolUnion.OfFunction; function != nil && function.Name == in.Call.Name {
			return temporal_runtime.NewTemporalTool(tool, a.broker, agents.ToolCallMiddlewaresOf(a.builder.Middlewares())...).Execute(ctx, in.Call)
		}
	}
	return nil, configError(fmt.Errorf("%w: %q has no sub-agent %q", agentconfig.ErrInvalid, in.Agent, in.Call.Name))
}

// subAgentProxy is a sub-agent tool in the workflow: described by the tool,
// run in a SubAgentCall activity carrying the snapshot. A retry would start
// the sub-agent over, so the activity runs once.
type subAgentProxy struct {
	ctx      workflow.Context
	snapshot *agentconfig.Snapshot
	agent    string
	tool     agents.Tool
}

func newSubAgentProxies(ctx workflow.Context, b *agentconfig.Builder, snapshot *agentconfig.Snapshot, agent string) []agents.Tool {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: SubAgentCallTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var proxies []agents.Tool
	for _, tool := range b.SubAgentTools(snapshot, agent) {
		proxies = append(proxies, &subAgentProxy{ctx: ctx, snapshot: snapshot, agent: agent, tool: tool})
	}
	return proxies
}

func (t *subAgentProxy) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var output *agents.ToolCallResponse
	err := workflow.ExecuteActivity(t.ctx, subAgentCallActivity, &SubAgentCallInput{Snapshot: t.snapshot, Agent: t.agent, Call: params}).Get(t.ctx, &output)
	if err != nil {
		return nil, err
	}
	return output, nil
}

func (t *subAgentProxy) GetToolDescriptor() *agents.BaseTool { return t.tool.GetToolDescriptor() }
