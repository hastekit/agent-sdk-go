package restate_runtime

import (
	"fmt"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
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

	agent := w.newRestateAgentProxy(restateCtx, agentOptions, input.ProviderConfigKey)

	// The proxy agent receives the broker via AgentOptions and publishes
	// chunks itself using StreamID. The caller's Execute owns the broker
	// stream's lifecycle (subscribe + close), so we don't close here.
	return agent.ExecuteWithoutTrace(restateCtx, &agents.AgentInput{
		Namespace:         input.Namespace,
		ThreadID:          input.ThreadID,
		PreviousMessageID: input.PreviousMessageID,
		Message:           input.Message,
		RunContext:        input.RunContext,
		StreamID:          streamID,
		PermissionMode:    input.PermissionMode,
	})
}

func (w *AgentWorkflow) newRestateAgentProxy(restateCtx restate.WorkflowContext, agentOptions *agents.AgentOptions, providerConfigKey string) *agents.Agent {
	promptProxy := NewRestatePrompt(restateCtx, agentOptions.Instruction)

	llmProxy := NewRestateLLM(restateCtx, agentOptions.LLM, providerConfigKey)

	conversationPersistenceProxy := NewRestateConversationPersistence(restateCtx, agentOptions.History.ConversationPersistenceAdapter)
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

	var restateTools []agents.Tool
	for _, tool := range agentOptions.Tools {
		restateTools = append(restateTools, NewRestateTool(restateCtx, tool, w.broker))
	}

	var mcpClients []agents.MCPToolset
	for _, mcpClient := range agentOptions.McpServers {
		mcpClients = append(mcpClients, NewRestateMCPServer(restateCtx, mcpClient, w.broker))
	}

	opts := &agents.AgentOptions{
		Name:       agentOptions.Name,
		Output:     agentOptions.Output,
		Parameters: agentOptions.Parameters,
		MaxLoops:   agentOptions.MaxLoops,

		Instruction:  promptProxy,
		History:      conversationHistory,
		Tools:        restateTools,
		McpServers:   mcpClients,
		ToolExecutor: NewRestateToolExecutor(restateCtx),
		// The agent's hooks apply to every tool, and each of their methods runs
		// as its own step.
		// The real hooks, each wrapped so its methods run as their own steps.
		Hooks:        restateHooks(restateCtx, agentOptions.Hooks),
		StreamBroker: NewRestateStreamBroker(restateCtx, w.broker),
		DurableStep:  NewRestateDurableStep(restateCtx),
	}

	for _, h := range agentOptions.Handoffs {
		agentOption := w.agentConfigs[h.Name]
		opts.Handoffs = append(opts.Handoffs, agents.NewHandoff(
			h.Name, h.Description, w.newRestateAgentProxy(restateCtx, agentOption, providerConfigKey),
		))
	}

	return agents.NewAgent(opts).WithLLM(llmProxy)
}
