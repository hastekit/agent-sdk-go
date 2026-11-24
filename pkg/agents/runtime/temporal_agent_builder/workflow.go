// Package temporal_agent_builder runs config-built agents (see agentconfig) on
// Temporal through one generic workflow, so an agent created after the worker
// started runs durably without registering anything.
//
// The workflow takes the agent's agentconfig.Snapshot as input and rebuilds it
// with proxies. Each proxy that depends on the config lives in its own file —
// the model call, the prompt, tools, MCP, skills and background tasks — and calls a fixed-name
// activity whose typed input carries the part of the config the worker
// rebuilds that dependency from: the model id, the instruction, the tool's
// catalog key, the MCP picks, the skill names. History, the broker and the
// stream heartbeat depend on no config, so temporal_runtime's own proxies and
// activities serve them. Every activity hands its rebuilt dependency to the
// same temporal_runtime activity code a static agent registers, so the two
// behave alike.
package temporal_agent_builder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// WorkflowName is the one workflow every config-built agent runs as, and the
// prefix of its activities. No static agent may take it as its name.
const WorkflowName = "AgentBuilder"

// TaskQueue is the queue the workflow runs on: the static agents' queue, so
// one worker serves both.
const TaskQueue = "AgentWorkflowTaskQueue"

// ConfigInvalidErrorType marks a failure no retry can fix: a snapshot that does
// not fit its run, or a config naming a model, tool or store the worker does
// not offer. See agentconfig.ErrInvalid.
const ConfigInvalidErrorType = "AgentConfigInvalid"

// configError makes a configuration failure non-retryable. Retrying would
// rebuild the same dependency from the same config and fail the same way,
// forever, under Temporal's default activity policy.
func configError(err error) error {
	if err == nil || !errors.Is(err, agentconfig.ErrInvalid) {
		return err
	}
	return temporal.NewNonRetryableApplicationError(err.Error(), ConfigInvalidErrorType, err)
}

// Registry is where the workflow and its activities are registered: a
// worker.Worker, or a testsuite.TestWorkflowEnvironment.
type Registry interface {
	worker.WorkflowRegistry
	worker.ActivityRegistry
}

// AgentBuilder is the worker side: the workflow and its activities, all served
// from one agentconfig.Builder. Register it once; every config, whenever it
// was written, runs through it.
type AgentBuilder struct {
	builder *agentconfig.Builder
	broker  agents.StreamBroker
}

// New serves builder's agents. broker must be the one the callers stream
// from — the runtime's.
func New(builder *agentconfig.Builder, broker agents.StreamBroker) *AgentBuilder {
	return &AgentBuilder{builder: builder, broker: broker}
}

// Register adds the workflow and every activity it schedules to r.
func (a *AgentBuilder) Register(r Registry) {
	r.RegisterWorkflowWithOptions(a.Execute, workflow.RegisterOptions{Name: WorkflowName})
	r.RegisterWorkflowWithOptions(a.BackgroundTask, workflow.RegisterOptions{Name: backgroundWorkflowName})

	// The config-dependent activities, each defined beside its proxy.
	r.RegisterActivityWithOptions(a.LLMCall, activity.RegisterOptions{Name: llmCallActivity})
	r.RegisterActivityWithOptions(a.GetPrompt, activity.RegisterOptions{Name: getPromptActivity})
	r.RegisterActivityWithOptions(a.ExecuteTool, activity.RegisterOptions{Name: executeToolActivity})
	r.RegisterActivityWithOptions(a.ListMCPTools, activity.RegisterOptions{Name: listMCPToolsActivity})
	r.RegisterActivityWithOptions(a.ExecuteMCPTool, activity.RegisterOptions{Name: executeMCPToolActivity})
	r.RegisterActivityWithOptions(a.ListSkills, activity.RegisterOptions{Name: listSkillsActivity})
	r.RegisterActivityWithOptions(a.ReadSkill, activity.RegisterOptions{Name: readSkillActivity})
	r.RegisterActivityWithOptions(a.AwaitTask, activity.RegisterOptions{Name: awaitTaskActivity})
	r.RegisterActivityWithOptions(a.SubAgentCall, activity.RegisterOptions{Name: subAgentCallActivity})

	// The rest depend on no config: temporal_runtime's activities, under the
	// names its proxies schedule with this workflow's name as their prefix.
	p := WorkflowName
	middlewares := a.builder.Middlewares()
	manager := a.builder.History()
	r.RegisterActivityWithOptions(temporal_runtime.NewStreamHeartbeatActivity(a.broker), activity.RegisterOptions{Name: p + temporal_runtime.HeartbeatActivitySuffix})

	persistence := temporal_runtime.NewTemporalConversationPersistence(manager.ConversationPersistenceAdapter, agents.HistoryMiddlewaresOf(middlewares)...)
	r.RegisterActivityWithOptions(persistence.LoadMessages, activity.RegisterOptions{Name: p + "_LoadMessagesActivity"})
	r.RegisterActivityWithOptions(persistence.SaveMessages, activity.RegisterOptions{Name: p + "_SaveMessagesActivity"})
	r.RegisterActivityWithOptions(persistence.SaveSummary, activity.RegisterOptions{Name: p + "_SaveSummaryActivity"})

	broker := temporal_runtime.NewTemporalStreamBroker(a.broker)
	r.RegisterActivityWithOptions(broker.IsStopped, activity.RegisterOptions{Name: p + "_IsStoppedActivity"})
	r.RegisterActivityWithOptions(broker.DrainMessages, activity.RegisterOptions{Name: p + "_DrainMessagesActivity"})
	r.RegisterActivityWithOptions(broker.WaitToolResult, activity.RegisterOptions{Name: p + "_WaitToolResultActivity"})

	delivery := temporal_runtime.NewTemporalBackgroundDelivery(a.broker)
	r.RegisterActivityWithOptions(delivery.CloseTaskStream, activity.RegisterOptions{Name: closeTaskStreamActivity})
	r.RegisterActivityWithOptions(delivery.DeliverTask, activity.RegisterOptions{Name: deliverTaskActivity})

	if manager.Summarizer != nil {
		r.RegisterActivityWithOptions(temporal_runtime.NewTemporalConversationSummarizer(manager.Summarizer).Summarize, activity.RegisterOptions{Name: p + "_SummarizerActivity"})
	}
	if manager.MessageFilter != nil {
		r.RegisterActivityWithOptions(temporal_runtime.NewTemporalMessageFilter(manager.MessageFilter).Filter, activity.RegisterOptions{Name: p + "_MessageFilterActivity"})
	}
}

// Execute runs the snapshot's root agent. A snapshot that does not fit the run
// — another namespace's, or one naming a tool this worker lacks — fails the
// workflow outright rather than any task retrying it.
func (a *AgentBuilder) Execute(ctx workflow.Context, snapshot *agentconfig.Snapshot, in *agents.AgentInput) (*agents.AgentOutput, error) {
	if in == nil {
		return nil, configError(fmt.Errorf("%w: no agent input", agentconfig.ErrInvalid))
	}
	if err := snapshot.Check(in.Namespace); err != nil {
		return nil, configError(err)
	}

	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
	})
	if in.StreamID == "" {
		in.StreamID = workflow.GetInfo(ctx).WorkflowExecution.ID
	}

	agent, err := a.buildAgent(ctx, snapshot, snapshot.Root, map[string]*agents.Agent{})
	if err != nil {
		return nil, configError(err)
	}

	// The loop runs on a plain context.Context, so carry the workflow's span
	// into it; see temporal_runtime.TemporalAgentV2.Execute.
	goCtx := context.Background()
	if span, ok := opentelemetry.SpanFromWorkflowContext(ctx); ok {
		goCtx = trace.ContextWithSpan(goCtx, span)
	}
	return agent.ExecuteWithoutTrace(goCtx, in)
}

// buildAgent builds one config's workflow-side agent, reusing any already
// built while walking the graph so a handoff cycle stays finite.
func (a *AgentBuilder) buildAgent(ctx workflow.Context, snapshot *agentconfig.Snapshot, name string, built map[string]*agents.Agent) (*agents.Agent, error) {
	if existing, ok := built[name]; ok {
		return existing, nil
	}
	cfg := snapshot.Agents[name]
	p := WorkflowName

	output, err := cfg.OutputSchema()
	if err != nil {
		return nil, err
	}

	manager := a.builder.History()
	var historyOptions []history.ConversationManagerOptions
	if manager.Summarizer != nil {
		historyOptions = append(historyOptions, history.WithSummarizer(temporal_runtime.NewTemporalConversationSummarizerProxy(ctx, p, manager.Summarizer)))
	}
	if manager.MessageFilter != nil {
		historyOptions = append(historyOptions, history.WithMessageFilter(temporal_runtime.NewTemporalMessageFilterProxy(ctx, p)))
	}

	// The catalog is code, the same on every worker, so reading a tool's
	// descriptor here is as deterministic as a static agent's tools are.
	tools := make([]agents.Tool, 0, len(cfg.Tools))
	for _, key := range cfg.Tools {
		tool, err := a.builder.Tool(key)
		if err != nil {
			return nil, err
		}
		tools = append(tools, newToolProxy(ctx, cfg.Name, key, tool))
	}
	tools = append(tools, newSubAgentProxies(ctx, a.builder, snapshot, cfg.Name)...)

	var mcpClient agents.MCPClient
	if a.builder.MCPClient(cfg.MCP) != nil {
		mcpClient = newMCPClientProxy(ctx, cfg.MCP)
	}
	var skillClient agents.SkillClient
	if a.builder.SkillClient(cfg.Skills) != nil {
		skillClient = newSkillClientProxy(ctx, cfg.Skills)
	}

	options := &agents.AgentOptions{
		Name:          cfg.Name,
		Output:        output,
		Parameters:    cfg.Parameters,
		StickyHandoff: cfg.StickyHandoff,

		History:      history.NewConversationManager(temporal_runtime.NewTemporalConversationPersistenceProxy(ctx, p), historyOptions...),
		Instruction:  newPromptProxy(ctx, cfg.Instruction),
		Tools:        tools,
		SkillClient:  skillClient,
		MCPClient:    mcpClient,
		ClientTools:  a.builder.ClientTools(),
		ToolExecutor: temporal_runtime.NewTemporalToolExecutor(ctx),
		StreamBroker: temporal_runtime.NewTemporalStreamBrokerProxy(ctx, p, a.broker),
		DurableStep:  temporal_runtime.NewTemporalDurableStep(ctx),
		// A task's wait outlives this run, so it goes to a workflow of its own
		// that carries the snapshot; see temporal_background.go.
		BackgroundRunner: &backgroundRunner{ctx: ctx, snapshot: snapshot},
	}
	if cfg.MaxLoops > 0 {
		maxLoops := cfg.MaxLoops
		options.MaxLoops = &maxLoops
	}

	agent := agents.NewAgent(options).WithLLM(newLLMProxy(ctx, cfg.Model))
	built[name] = agent

	for _, h := range cfg.Handoffs {
		if _, ok := snapshot.Agents[h.Name]; !ok {
			slog.Warn("handoff target is not in the agent snapshot; the edge will not exist in this run",
				slog.String("agent", cfg.Name), slog.String("target", h.Name))
			continue
		}
		target, err := a.buildAgent(ctx, snapshot, h.Name, built)
		if err != nil {
			return nil, err
		}
		agent.AddHandoffs(agents.NewHandoff(h.Name, h.Description, target))
	}
	return agent, nil
}
