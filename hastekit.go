package sdk

import (
	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

var (
	agentsByName         = map[string]*agents.Agent{}
	temporalAgentConfigs = map[string]*agents.AgentOptions{}
	restateAgentConfigs  = map[string]*agents.AgentOptions{}

	DefaultStreamBroker = streambroker.NewMemoryStreamBroker()
)

type Agent = agents.Agent
type ModelParameters = responses.Parameters

// Hook is anything an agent can be given to observe or intercept what it does
// — see agents.Hook. Implement ToolCallHook, ModelCallHook, or both.
type Hook = agents.Hook

// ToolCallHook wraps a tool call — see agents.ToolCallHook.
type ToolCallHook = agents.ToolCallHook

// ModelCallHook wraps a call to the model — see agents.ModelCallHook.
type ModelCallHook = agents.ModelCallHook

type AgentConfig struct {
	Name          string
	LLM           llm.Provider
	Output        map[string]any
	Tools         []Tool
	Handoffs      []*agents.Handoff
	McpServers    []agents.MCPToolset
	MaxLoops      *int
	History       *history.CommonConversationManager
	Instruction   agents.SystemPromptProvider
	Parameters    responses.Parameters
	StickyHandoff bool

	// Hooks observe or intercept what the agent does. A ToolCallHook wraps
	// every tool it calls; a ModelCallHook wraps every call to the model, which
	// is where a budget or credit check belongs. One hook may be both.
	Hooks []agents.Hook
}

func (ac *AgentConfig) toAgentOptions() *agents.AgentOptions {
	return &agents.AgentOptions{
		Name:          ac.Name,
		LLM:           ac.LLM,
		Output:        ac.Output,
		Tools:         ac.Tools,
		Handoffs:      ac.Handoffs,
		McpServers:    ac.McpServers,
		MaxLoops:      ac.MaxLoops,
		History:       ac.History,
		Instruction:   ac.Instruction,
		Parameters:    ac.Parameters,
		StickyHandoff: ac.StickyHandoff,
		Hooks:         ac.Hooks,
	}
}

// NewAgent creates a new agent with the given configuration
func NewAgent(cfg *AgentConfig, opts ...AgentOption) *Agent {
	// Convert to AgentOptions
	agentOptions := cfg.toAgentOptions()

	// Apply options
	for _, opt := range opts {
		opt(agentOptions)
	}

	if agentOptions.StreamBroker == nil {
		agentOptions.StreamBroker = DefaultStreamBroker
	}

	// Create the agent
	agent := agents.NewAgent(agentOptions)

	// Add to the SDK
	agentsByName[agentOptions.Name] = agent

	return agent
}

type AgentOption func(options *agents.AgentOptions)

func WithRuntime(runtime agents.Runtime, broker agents.StreamBroker) AgentOption {
	return func(opts *agents.AgentOptions) {
		switch runtime.(type) {
		case *TemporalRuntime:
			temporalAgentConfigs[opts.Name] = opts
		case *RestateRuntime:
			restateAgentConfigs[opts.Name] = opts
		default:
		}

		opts.Runtime = runtime
		opts.StreamBroker = broker
	}
}
