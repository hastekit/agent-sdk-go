package temporal_runtime

import (
	"context"
	"maps"
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

	temporalPrompt := NewTemporalPrompt(a.options.Instruction)
	activities[a.options.Name+"_GetPromptActivity"] = temporalPrompt.GetPrompt

	temporalLLM := NewTemporalLLM(a.options.LLM, a.broker)
	activities[a.options.Name+"_NewStreamingResponsesActivity"] = temporalLLM.NewStreamingResponsesActivity

	temporalConversationPersistence := NewTemporalConversationPersistence(a.options.History.ConversationPersistenceAdapter)
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
		temporalTool := NewTemporalTool(tool, a.broker)
		activities[getToolName(a.options.Name, tool)+"_ExecuteToolActivity"] = temporalTool.Execute
	}

	// Four activities per hook, so the workflow can run each method as its own
	// step.
	maps.Copy(activities, hookActivities(a.options.Name, a.options.Hooks))

	for _, mcpClient := range a.options.McpServers {
		temporalMCP := NewTemporalMCPServer(mcpClient, a.broker)
		activities[mcpClient.GetName()+"_ListMCPToolsActivity"] = temporalMCP.ListTools
		activities[mcpClient.GetName()+"_ExecuteMCPToolActivity"] = temporalMCP.ExecuteTool
	}

	return activities
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
		mcpProxy := NewTemporalMCPProxy(ctx, mcpClient.GetName())
		mcpProxies = append(mcpProxies, mcpProxy)
	}

	opts := &agents.AgentOptions{
		Name:       a.options.Name,
		Output:     a.options.Output,
		Parameters: a.options.Parameters,
		MaxLoops:   a.options.MaxLoops,

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
		// Proxies, not the hooks themselves: the executor and the loop both run
		// in the workflow, so each hook method becomes its own activity.
		Hooks:        hookProxies(ctx, a.options.Name, a.options.Hooks),
		StreamBroker: NewTemporalStreamBrokerProxy(ctx, a.options.Name, a.broker),
		DurableStep:  NewTemporalDurableStep(ctx),
	}

	for _, h := range a.options.Handoffs {
		agentOptions := a.agentConfigs[h.Name]
		opts.Handoffs = append(opts.Handoffs, agents.NewHandoff(
			h.Name, h.Description, NewTemporalAgent(a.agentConfigs, agentOptions, a.broker).newTemporalProxyAgent(ctx),
		))
	}

	return agents.NewAgent(opts).WithLLM(llmProxy)
}

func getToolName(prefix string, tool agents.Tool) string {
	toolName := ""
	if t := tool.Tool(context.Background()); t != nil {
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
