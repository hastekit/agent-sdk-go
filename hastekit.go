package sdk

import (
	"fmt"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"strings"
)

type Agent = agents.Agent
type ModelParameters = responses.Parameters

// AgentMiddleware wraps agent operations. Embed agents.NoopMiddleware and
// override the Wrap methods relevant to your implementation. The root
// Middleware alias remains the gateway/provider middleware API.
type AgentMiddleware = agents.Middleware

// ToolCallMiddleware wraps a tool call — see agents.ToolCallMiddleware.
type ToolCallMiddleware = agents.ToolCallMiddleware

// ModelCallMiddleware wraps a call to the model — see agents.ModelCallMiddleware.
type ModelCallMiddleware = agents.ModelCallMiddleware
type HistoryMiddleware = agents.HistoryMiddleware
type PromptMiddleware = agents.PromptMiddleware

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

	// Skills list runtime catalogs and resolve enabled skills through read_skill.
	Skills []agents.SkillSet

	// Middlewares wrap model/tool calls, history loads/saves and prompt
	// retrieval. Embed agents.NoopMiddleware and override selected methods.
	Middlewares []agents.Middleware
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
		Skills:        ac.Skills,
		Middlewares:   ac.Middlewares,
	}
}

// NewAgent validates configuration, constructs an agent, and registers its
// resolved configuration with the supplied runtime before returning.
// History, runtime and broker resources remain owned by the caller.
func NewAgent(cfg *AgentConfig, opts ...AgentOption) (*Agent, error) {
	agent, options, err := buildAgent(cfg, opts...)
	if err != nil {
		return nil, err
	}
	if options.Runtime != nil {
		if err := options.Runtime.RegisterAgent(options); err != nil {
			return nil, err
		}
	}
	return agent, nil
}

func buildAgent(cfg *AgentConfig, opts ...AgentOption) (*Agent, *agents.AgentOptions, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("agent configuration is nil")
	}
	options := cfg.toAgentOptions()
	for _, opt := range opts {
		if opt == nil {
			return nil, nil, fmt.Errorf("nil agent option")
		}
		opt(options)
	}
	if strings.TrimSpace(options.Name) == "" {
		return nil, nil, fmt.Errorf("agent name is required")
	}
	if err := agents.ValidateSkillSets(options.Skills); err != nil {
		return nil, nil, err
	}
	if options.LLM == nil {
		return nil, nil, fmt.Errorf("agent model is required")
	}
	if options.MaxLoops != nil && *options.MaxLoops < 0 {
		return nil, nil, fmt.Errorf("MaxLoops must not be negative")
	}
	if options.StreamBroker == nil {
		if options.Runtime != nil {
			return nil, nil, fmt.Errorf("an explicit stream broker is required when configuring a runtime")
		}
		options.StreamBroker = streambroker.NewMemoryStreamBroker()
	}
	return agents.NewAgent(options), options, nil
}

// MustNewAgent is NewAgent for static configuration where failure is a programmer error.
func MustNewAgent(cfg *AgentConfig, opts ...AgentOption) *Agent {
	agent, err := NewAgent(cfg, opts...)
	if err != nil {
		panic(err)
	}
	return agent
}

type AgentOption func(options *agents.AgentOptions)

// WithRuntime selects a runtime and uses its StreamBroker. A nil runtime uses
// local execution with a default in-memory broker. NewAgent registers the resolved
// configuration with the runtime; start Temporal/Restate workers with Serve.
func WithRuntime(runtime agents.Runtime) AgentOption {
	return func(opts *agents.AgentOptions) {
		opts.Runtime = runtime
		opts.StreamBroker = nil
		if runtime != nil {
			opts.StreamBroker = runtime.StreamBroker()
		}
	}
}
