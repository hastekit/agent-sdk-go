package restate_agent_builder

import (
	"context"
	"errors"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	restate "github.com/restatedev/sdk-go"
)

// BackgroundServiceName is the service that waits for config-built agents'
// background tasks, one invocation per task.
const BackgroundServiceName = "AgentBuilderBackground"

// BackgroundInput is what the background service is invoked with: which
// catalog tool to ask about the task, and the snapshot a run started by its
// result enters, as the run that started the task saw it.
type BackgroundInput struct {
	Snapshot *agentconfig.Snapshot    `json:"snapshot"`
	Agent    string                   `json:"agent"`
	Tool     string                   `json:"tool"`
	Ref      agents.BackgroundTaskRef `json:"ref"`

	// ProviderConfigKey travels with the task as it travels with a run:
	// Restate has no context propagator.
	ProviderConfigKey string `json:"provider_config_key,omitempty"`
}

// Background waits for config-built agents' background tasks and delivers
// their results. It is the config-built counterpart of
// restate_runtime.BackgroundTaskService: it finds the tool by catalog key
// instead of by registered agent, and a result that starts a run starts the
// AgentBuilder workflow with the task's snapshot.
type Background struct {
	builder *agentconfig.Builder
	broker  agents.StreamBroker
}

// NewBackground serves builder's background tasks. broker must be the
// runtime's.
func NewBackground(builder *agentconfig.Builder, broker agents.StreamBroker) *Background {
	return &Background{builder: builder, broker: broker}
}

// ServiceName names the service for restate.Reflect.
func (b *Background) ServiceName() string { return BackgroundServiceName }

// Await waits for one task, then puts its outcome in front of the agent.
func (b *Background) Await(ctx restate.Context, in *BackgroundInput) (restate.Void, error) {
	if in == nil || in.Snapshot == nil {
		return restate.Void{}, restate.TerminalError(fmt.Errorf("%w: no background task input", agentconfig.ErrInvalid))
	}
	tool, err := b.tool(in.Agent, in.Tool)
	if err != nil {
		return restate.Void{}, restate.TerminalError(err)
	}

	decision, err := restate_runtime.AwaitBackgroundTask(ctx, b.broker, tool, in.Ref, in.Tool)
	if err != nil || !decision.Start {
		return restate.Void{}, err
	}

	// The thread was idle and this invocation holds its claim: run the
	// snapshot's agent one-way, under a key of its own.
	restate.WorkflowSend(ctx, ServiceName, restate.UUID(ctx).String(), "Run").Send(&Input{
		Snapshot: in.Snapshot,
		WorkflowInput: restate_runtime.WorkflowInput{
			AgentName:         in.Snapshot.Root,
			Namespace:         in.Ref.Namespace,
			ThreadID:          in.Ref.ThreadID,
			SessionID:         in.Ref.SessionID,
			Message:           decision.Message,
			RunContext:        in.Ref.RunContext,
			Skills:            in.Ref.Skills,
			MCP:               in.Ref.MCP,
			ClientTools:       in.Ref.ClientTools,
			StreamID:          in.Ref.ThreadStreamID,
			ProviderConfigKey: in.ProviderConfigKey,
		},
	})
	return restate.Void{}, nil
}

// tool resolves the catalog tool that started the task, with the Builder's
// tool-call middlewares around its wait.
func (b *Background) tool(agent, key string) (agents.BackgroundTool, error) {
	tool, err := b.builder.Tool(key)
	if err != nil {
		return nil, err
	}
	background, ok := tool.(agents.BackgroundTool)
	if !ok {
		return nil, fmt.Errorf("%w: catalog tool %q starts no background tasks", agentconfig.ErrInvalid, key)
	}
	return agents.WrapBackgroundTool(agent, background, agents.ToolCallMiddlewaresOf(b.builder.Middlewares())...), nil
}

// backgroundTool is a catalog tool that starts background tasks, as the
// workflow runs it: the loop asks it whether a task id is allowed, and the
// runner reads its catalog key to hand the wait to the background service.
type backgroundTool struct {
	*restate_runtime.RestateTool
	agent string
	key   string
}

var _ agents.BackgroundTool = (*backgroundTool)(nil)

// AwaitTask is never called in the workflow: the background service waits.
func (t *backgroundTool) AwaitTask(context.Context, agents.BackgroundTaskRef, agents.ProgressReporter) (agents.BackgroundResult, error) {
	return agents.BackgroundResult{}, errors.New("a background task is waited on by its own invocation, not inside the run that started it")
}

// backgroundRunner hands a task's wait to the background service, one-way, so
// it outlives the run that started it.
type backgroundRunner struct {
	ctx               restate.WorkflowContext
	snapshot          *agentconfig.Snapshot
	providerConfigKey string
}

var _ agents.BackgroundRunner = (*backgroundRunner)(nil)

func (r *backgroundRunner) StartTask(_ context.Context, tool agents.BackgroundTool, ref agents.BackgroundTaskRef) error {
	proxy, ok := tool.(*backgroundTool)
	if !ok {
		return fmt.Errorf("background tool for task %s is not a config-built agent's catalog tool", ref.TaskID)
	}
	restate.ServiceSend(r.ctx, BackgroundServiceName, "Await").Send(&BackgroundInput{
		Snapshot:          r.snapshot,
		Agent:             proxy.agent,
		Tool:              proxy.key,
		Ref:               ref,
		ProviderConfigKey: r.providerConfigKey,
	})
	return nil
}
