package temporal_runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/workflow"
)

type TemporalAgentV2 struct {
	agentConfigs map[string]*agents.AgentOptions
	options      *agents.AgentOptions
	broker       agents.StreamBroker
}

func NewTemporalAgent(configs map[string]*agents.AgentOptions, options *agents.AgentOptions, broker agents.StreamBroker) *TemporalAgentV2 {
	return &TemporalAgentV2{
		agentConfigs: configs,
		options:      options,
		broker:       broker,
	}
}

func (a *TemporalAgentV2) GetActivities() map[string]interface{} {
	activities := map[string]interface{}{}

	temporalPrompt := NewTemporalPrompt(a.options.Instruction, agents.PromptMiddlewaresOf(a.options.Middlewares)...)
	activities[a.options.Name+"_GetPromptActivity"] = temporalPrompt.GetPrompt

	temporalLLM := NewTemporalLLM(a.options.LLM, a.broker, agents.ModelCallMiddlewaresOf(a.options.Middlewares)...)
	activities[a.options.Name+"_NewStreamingResponsesActivity"] = temporalLLM.NewStreamingResponsesActivity

	temporalConversationPersistence := NewTemporalConversationPersistence(a.options.History.ConversationPersistenceAdapter, agents.HistoryMiddlewaresOf(a.options.Middlewares)...)
	activities[a.options.Name+"_LoadMessagesActivity"] = temporalConversationPersistence.LoadMessages
	activities[a.options.Name+"_SaveMessagesActivity"] = temporalConversationPersistence.SaveMessages
	activities[a.options.Name+"_SaveSummaryActivity"] = temporalConversationPersistence.SaveSummary

	temporalStreamBroker := NewTemporalStreamBroker(a.broker)
	activities[a.options.Name+"_IsStoppedActivity"] = temporalStreamBroker.IsStopped
	activities[a.options.Name+"_DrainMessagesActivity"] = temporalStreamBroker.DrainMessages

	if a.options.History.Summarizer != nil {
		temporalSummarizer := NewTemporalConversationSummarizer(a.options.History.Summarizer)
		activities[a.options.Name+"_SummarizerActivity"] = temporalSummarizer
	}

	if a.options.History.MessageFilter != nil {
		temporalMessageFilter := NewTemporalMessageFilter(a.options.History.MessageFilter)
		activities[a.options.Name+"_MessageFilterActivity"] = temporalMessageFilter.Filter
	}

	// WithSkillTool, not options.Tools: an agent given skills adds the tool that
	// reads them itself, and a tool with no activity registered is one the
	// workflow cannot call.
	for _, tool := range agents.WithSkillTool(a.options.Tools, a.options.Skills) {
		temporalTool := NewTemporalTool(tool, a.broker, agents.ToolCallMiddlewaresOf(a.options.Middlewares)...)
		activities[getToolName(a.options.Name, tool)+"_ExecuteToolActivity"] = temporalTool.Execute

		// A tool that starts background tasks needs a second activity: the one
		// the task's own workflow waits in, long after this run is over.
		if backgroundTool, ok := tool.(agents.BackgroundTool); ok {
			temporalBackground := NewTemporalBackgroundTask(agents.WrapBackgroundTool(a.options.Name, backgroundTool, agents.ToolCallMiddlewaresOf(a.options.Middlewares)...), a.broker)
			activities[getToolName(a.options.Name, tool)+awaitTaskActivitySuffix] = temporalBackground.AwaitTask
		}
	}

	// Closing a task's stream and deciding where its result goes are the same
	// two steps whichever tool started it, so they are registered per agent.
	temporalDelivery := NewTemporalBackgroundDelivery(a.broker)
	activities[a.options.Name+closeTaskStreamActivityName] = temporalDelivery.CloseTaskStream
	activities[a.options.Name+deliverTaskActivityName] = temporalDelivery.DeliverTask

	for _, mcpClient := range a.options.McpServers {
		temporalMCP := NewTemporalMCPServer(mcpClient, a.broker, agents.ToolCallMiddlewaresOf(a.options.Middlewares)...)
		prefix := a.options.Name + "_" + mcpClient.GetName()
		activities[prefix+"_ListMCPToolsActivity"] = temporalMCP.ListTools
		activities[prefix+"_ExecuteMCPToolActivity"] = temporalMCP.ExecuteTool
	}

	return activities
}

// GetWorkflows returns the workflows to register for this agent, by name.
func (a *TemporalAgentV2) GetWorkflows() map[string]any {
	return map[string]any{
		a.options.Name + "_AgentWorkflow":         a.Execute,
		a.options.Name + backgroundWorkflowSuffix: NewBackgroundTaskWorkflow(a.options.Name).Execute,
	}
}

func (a *TemporalAgentV2) Execute(ctx workflow.Context, in *agents.AgentInput) (*agents.AgentOutput, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
	})

	// Fall back to the workflow execution ID when the caller didn't set
	// a StreamID. The proxy agent receives the broker via AgentOptions
	// and publishes through it using in.StreamID.
	if in.StreamID == "" {
		in.StreamID = workflow.GetInfo(ctx).WorkflowExecution.ID
	}

	agent := a.newTemporalProxyAgent(ctx)

	// The agent loop runs on a plain context.Context (workflow.Context can't
	// cross into it), so bridge the workflow's OpenTelemetry span into that
	// context. Without this, any spans the loop creates start with no parent
	// and land in a disconnected trace instead of nesting under the Temporal
	// workflow span. The span context is deterministic across replays, so this
	// is replay-safe.
	goCtx := context.Background()
	if span, ok := opentelemetry.SpanFromWorkflowContext(ctx); ok {
		goCtx = trace.ContextWithSpan(goCtx, span)
	}

	return agent.ExecuteWithoutTrace(goCtx, in)
}

func (a *TemporalAgentV2) newTemporalProxyAgent(ctx workflow.Context) *agents.Agent {
	return a.proxyAgent(ctx, map[string]*agents.Agent{})
}

// proxyAgent builds the workflow-side agent, reusing any it has already built
// while walking this graph.
//
// built is what makes a cycle finite. Handoffs are a graph, not a tree — a
// specialist that can hand back to the agent that called it is the ordinary
// shape — and rebuilding each target in turn walked that cycle until the stack
// ran out. Registering an agent before wiring its edges is what breaks it: the
// second visit finds the one already under construction and points at that,
// so the cycle exists in the rebuilt graph exactly as it does in the original.
func (a *TemporalAgentV2) proxyAgent(ctx workflow.Context, built map[string]*agents.Agent) *agents.Agent {
	if existing, ok := built[a.options.Name]; ok {
		return existing
	}

	promptProxy := NewTemporalPromptProxy(ctx, a.options.Name)

	llmProxy := NewTemporalLLMProxy(ctx, a.options.Name, a.broker)

	conversationPersistenceProxy := NewTemporalConversationPersistenceProxy(ctx, a.options.Name)
	var options []history.ConversationManagerOptions
	if a.options.History.Summarizer != nil {
		conversationSummarizerProxy := NewTemporalConversationSummarizerProxy(ctx, a.options.Name)
		options = append(options, history.WithSummarizer(conversationSummarizerProxy))
	}
	if a.options.History.MessageFilter != nil {
		conversationFilterProxy := NewTemporalMessageFilterProxy(ctx, a.options.Name)
		options = append(options, history.WithMessageFilter(conversationFilterProxy))
	}
	conversationHistory := history.NewConversationManager(conversationPersistenceProxy, options...)

	var toolProxies []agents.Tool
	for _, tool := range agents.WithSkillTool(a.options.Tools, a.options.Skills) {
		toolProxy := NewTemporalToolProxy(ctx, getToolName(a.options.Name, tool), tool)
		toolProxies = append(toolProxies, toolProxy)
	}

	var mcpProxies []agents.MCPToolset
	for _, mcpClient := range a.options.McpServers {
		mcpProxy := &TemporalMCPProxy{workflowCtx: ctx, name: mcpClient.GetName(), prefix: a.options.Name + "_" + mcpClient.GetName()}
		mcpProxies = append(mcpProxies, mcpProxy)
	}

	opts := &agents.AgentOptions{
		Name:       a.options.Name,
		Output:     a.options.Output,
		Parameters: a.options.Parameters,
		MaxLoops:   a.options.MaxLoops,
		// Behaviour the agent was configured with, and which the workflow
		// rebuild has to carry: a field left out here does not fail, it just
		// stops applying inside a workflow. Sticky routing went missing that
		// way, and nothing said so.
		StickyHandoff: a.options.StickyHandoff,
		SingleTurn:    a.options.SingleTurn,

		History:     conversationHistory,
		Instruction: promptProxy,
		Tools:       toolProxies,
		// The skills travel with the proxy agent so the prompt still lists
		// them, and names the tool that reads them. The reader tool itself is
		// already in toolProxies, wrapped as a workflow step — the agent sees
		// it there and does not add a second, unjournaled one.
		Skills:       a.options.Skills,
		McpServers:   mcpProxies,
		ToolExecutor: NewTemporalToolExecutor(ctx),
		StreamBroker: NewTemporalStreamBrokerProxy(ctx, a.options.Name, a.broker),
		DurableStep:  NewTemporalDurableStep(ctx),
		// A task's wait outlives this run, so it goes to a workflow of its own
		// rather than a goroutine that would die with the activity.
		BackgroundRunner: NewTemporalBackgroundRunner(ctx, a.options.Name),
	}

	// Built before its edges are wired, and recorded straight away: a target
	// that hands back to this agent has to find it here rather than start
	// building it again.
	agent := agents.NewAgent(opts).WithLLM(llmProxy)
	built[a.options.Name] = agent

	for _, h := range a.options.Handoffs {
		agentOptions := a.agentConfigs[h.Name]
		if agentOptions == nil {
			// A target that was never registered with this runtime. Rebuilding
			// it would dereference nothing and fail the workflow task, which
			// Temporal then retries forever — so the edge is dropped and said
			// aloud instead.
			slog.Warn("handoff target is not registered with the runtime; the edge will not exist in durable runs",
				slog.String("agent", a.options.Name), slog.String("target", h.Name))
			continue
		}
		agent.AddHandoffs(agents.NewHandoff(
			h.Name, h.Description,
			NewTemporalAgent(a.agentConfigs, agentOptions, a.broker).proxyAgent(ctx, built),
		))
	}

	return agent
}

func getToolName(prefix string, tool agents.Tool) string {
	toolName := ""
	if descriptor := tool.GetToolDescriptor(); descriptor != nil {
		t := descriptor.ToolUnion
		if t.OfFunction != nil {
			toolName = t.OfFunction.Name
		}

		if t.OfWebSearch != nil {
			toolName = "web_search"
		}

		if t.OfImageGeneration != nil {
			toolName = "image_generation"
		}
	}

	return prefix + "_" + toolName
}
