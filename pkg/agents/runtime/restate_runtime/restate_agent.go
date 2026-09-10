package restate_runtime

import (
	"fmt"
	"log/slog"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	restate "github.com/restatedev/sdk-go"
)

// AgentWorkflow is the Restate workflow that executes agents with durability.
type AgentWorkflow struct {
	agentConfigs map[string]*agents.AgentOptions
	broker       agents.StreamBroker
}

func NewRestateWorkflow(agentConfigs map[string]*agents.AgentOptions, broker agents.StreamBroker) *AgentWorkflow {
	return &AgentWorkflow{
		agentConfigs: agentConfigs,
		broker:       broker,
	}
}

// Run executes the agent inside a Restate workflow context.
func (w *AgentWorkflow) Run(restateCtx restate.WorkflowContext, input *WorkflowInput) (*agents.AgentOutput, error) {
	agentOptions, ok := w.agentConfigs[input.AgentName]
	if !ok {
		return &agents.AgentOutput{Status: agentstate.RunStatusError}, fmt.Errorf("agent not found: %s", input.AgentName)
	}

	// Prefer the StreamID supplied by the caller. The Restate runtime
	// sets it equal to the workflow key, so falling back to the key
	// keeps older callers working.
	streamID := input.StreamID
	if streamID == "" {
		streamID = restate.Key(restateCtx)
	}

	agent := w.newRestateAgentProxy(restateCtx, agentOptions, input.ProviderConfigKey, streamID)

	// The proxy agent receives the broker via AgentOptions and publishes
	// chunks itself using StreamID. The caller's Execute owns the broker
	// stream's lifecycle (subscribe + close), so we don't close here.
	return agent.ExecuteWithoutTrace(restateCtx, &agents.AgentInput{
		Namespace:     input.Namespace,
		ThreadID:      input.ThreadID,
		PreviousRunID: input.PreviousRunID,
		Message:       input.Message,
		RunContext:    input.RunContext,
		StreamID:      streamID,
	})
}

func (w *AgentWorkflow) newRestateAgentProxy(restateCtx restate.WorkflowContext, agentOptions *agents.AgentOptions, providerConfigKey string, streamID string) *agents.Agent {
	return w.proxyAgent(restateCtx, agentOptions, providerConfigKey, streamID, map[string]*agents.Agent{})
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
func (w *AgentWorkflow) proxyAgent(
	restateCtx restate.WorkflowContext,
	agentOptions *agents.AgentOptions,
	providerConfigKey string,
	streamID string,
	built map[string]*agents.Agent,
) *agents.Agent {
	if existing, ok := built[agentOptions.Name]; ok {
		return existing
	}

	promptProxy := NewRestatePrompt(restateCtx, agentOptions.Instruction, agents.PromptMiddlewaresOf(agentOptions.Middlewares)...)

	llmProxy := NewRestateLLM(restateCtx, agentOptions.LLM, providerConfigKey, w.broker, streamID, agents.ModelCallMiddlewaresOf(agentOptions.Middlewares)...)

	conversationPersistenceProxy := NewRestateConversationPersistence(restateCtx, agentOptions.History.ConversationPersistenceAdapter, agents.HistoryMiddlewaresOf(agentOptions.Middlewares)...)
	var options []history.ConversationManagerOptions
	if agentOptions.History.Summarizer != nil {
		conversationSummarizerProxy := NewRestateConversationSummarizer(restateCtx, agentOptions.History.Summarizer)
		options = append(options, history.WithSummarizer(conversationSummarizerProxy))
	}
	if agentOptions.History.MessageFilter != nil {
		conversationFilterProxy := NewRestateMessageFilter(restateCtx, agentOptions.History.MessageFilter)
		options = append(options, history.WithMessageFilter(conversationFilterProxy))
	}
	conversationHistory := history.NewConversationManager(conversationPersistenceProxy, options...)

	// WithSkillTool, not options.Tools: an agent given skills adds the tool that
	// reads them itself, and a tool this loop never wraps is one that would
	// run outside the workflow's journal.
	var restateTools []agents.Tool
	for _, tool := range agents.WithSkillTool(agentOptions.Tools, agentOptions.Skills) {
		restateTools = append(restateTools, newRestateTool(restateCtx, agentOptions.Name, tool, w.broker, agents.ToolCallMiddlewaresOf(agentOptions.Middlewares)...))
	}

	var mcpClients []agents.MCPToolset
	for _, mcpClient := range agentOptions.McpServers {
		mcpClients = append(mcpClients, NewRestateMCPServer(restateCtx, mcpClient, w.broker, agents.ToolCallMiddlewaresOf(agentOptions.Middlewares)...))
	}

	opts := &agents.AgentOptions{
		Name:       agentOptions.Name,
		Output:     agentOptions.Output,
		Parameters: agentOptions.Parameters,
		MaxLoops:   agentOptions.MaxLoops,
		// Behaviour the agent was configured with, and which the workflow
		// rebuild has to carry: a field left out here does not fail, it just
		// stops applying inside a workflow. Sticky routing went missing that
		// way, and nothing said so.
		StickyHandoff: agentOptions.StickyHandoff,
		SingleTurn:    agentOptions.SingleTurn,

		Instruction: promptProxy,
		History:     conversationHistory,
		Tools:       restateTools,
		// The skills travel with the proxy agent so the prompt still lists
		// them, and names the tool that reads them. The reader tool itself is
		// already in restateTools, wrapped as a workflow step — the agent sees
		// it there and does not add a second, unjournaled one.
		Skills:       agentOptions.Skills,
		McpServers:   mcpClients,
		ToolExecutor: NewRestateToolExecutor(restateCtx),
		StreamBroker: NewRestateStreamBroker(restateCtx, w.broker),
		DurableStep:  NewRestateDurableStep(restateCtx),
		// A task's wait outlives this run, so it goes to an invocation of its
		// own rather than a goroutine that would die with the step.
		BackgroundRunner: NewRestateBackgroundRunner(restateCtx, agentOptions.Name, providerConfigKey),
	}

	// Built before its edges are wired, and recorded straight away: a target
	// that hands back to this agent has to find it here rather than start
	// building it again.
	agent := agents.NewAgent(opts).WithLLM(llmProxy)
	built[agentOptions.Name] = agent

	for _, h := range agentOptions.Handoffs {
		agentOption := w.agentConfigs[h.Name]
		if agentOption == nil {
			// A target that was never registered with this runtime. Rebuilding
			// it would dereference nothing and fail the invocation, which
			// Restate then retries — so the edge is dropped and said aloud
			// instead.
			slog.Warn("handoff target is not registered with the runtime; the edge will not exist in durable runs",
				slog.String("agent", agentOptions.Name), slog.String("target", h.Name))
			continue
		}
		agent.AddHandoffs(agents.NewHandoff(
			h.Name, h.Description,
			w.proxyAgent(restateCtx, agentOption, providerConfigKey, streamID, built),
		))
	}

	return agent
}
