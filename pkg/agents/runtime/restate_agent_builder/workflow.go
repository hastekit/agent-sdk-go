// Package restate_agent_builder runs config-built agents (see agentconfig) on
// Restate through one generic workflow, so an agent created after the service
// started runs durably without registering anything.
//
// The workflow takes the agent's agentconfig.Snapshot as input. Restate's
// durable steps wrap the real dependency in-process, so the workflow rebuilds
// each dependency from the config with the service's agentconfig.Builder —
// pure construction, so a replay rebuilds the same graph — and wraps it in
// restate_runtime's proxies, the same ones a static agent runs through.
package restate_agent_builder

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	restate "github.com/restatedev/sdk-go"
)

// ServiceName is the Restate workflow every config-built agent runs as,
// through its Run handler.
const ServiceName = "AgentBuilder"

// Input is the workflow's input: the run, as restate_runtime.WorkflowInput
// carries it, plus the snapshot of the agent to run.
type Input struct {
	Snapshot *agentconfig.Snapshot `json:"snapshot"`
	restate_runtime.WorkflowInput
}

// AgentBuilder is the Restate workflow for config-built agents, served from
// one agentconfig.Builder.
type AgentBuilder struct {
	builder *agentconfig.Builder
	broker  agents.StreamBroker
}

// New serves builder's agents. broker must be the one the callers stream
// from — the runtime's.
func New(builder *agentconfig.Builder, broker agents.StreamBroker) *AgentBuilder {
	return &AgentBuilder{builder: builder, broker: broker}
}

// ServiceName names the service for restate.Reflect.
func (b *AgentBuilder) ServiceName() string { return ServiceName }

// Run executes the snapshot's root agent. A snapshot that does not fit the run
// — another namespace's, or one naming a model or tool this service does not
// offer — is a terminal error, which Restate does not retry.
func (b *AgentBuilder) Run(ctx restate.WorkflowContext, input *Input) (*agents.AgentOutput, error) {
	if input == nil {
		return nil, restate.TerminalError(fmt.Errorf("%w: no agent input", agentconfig.ErrInvalid))
	}
	if err := input.Snapshot.Check(input.Namespace); err != nil {
		return nil, restate.TerminalError(err)
	}
	streamID := input.StreamID
	if streamID == "" {
		streamID = restate.Key(ctx)
	}
	agent, err := b.buildAgent(ctx, input, streamID, input.Snapshot.Root, map[string]*agents.Agent{})
	if err != nil {
		if errors.Is(err, agentconfig.ErrInvalid) {
			err = restate.TerminalError(err)
		}
		return nil, err
	}
	return agent.ExecuteWithoutTrace(ctx, &agents.AgentInput{
		Namespace:     input.Namespace,
		GroupID:       input.GroupID,
		RunID:         input.RunID,
		ThreadID:      input.ThreadID,
		SessionID:     input.SessionID,
		PreviousRunID: input.PreviousRunID,
		Message:       input.Message,
		RunContext:    input.RunContext,
		Skills:        input.Skills,
		MCP:           input.MCP,
		ClientTools:   input.ClientTools,
		StreamID:      streamID,
	})
}

// buildAgent rebuilds one config from the Builder's dependencies, each wrapped
// in its durable step, reusing any agent already built while walking the
// graph so a handoff cycle stays finite.
func (b *AgentBuilder) buildAgent(ctx restate.WorkflowContext, input *Input, streamID, name string, built map[string]*agents.Agent) (*agents.Agent, error) {
	if existing, ok := built[name]; ok {
		return existing, nil
	}
	cfg := input.Snapshot.Agents[name]
	real, err := b.builder.AgentOptions(cfg)
	if err != nil {
		return nil, err
	}
	middlewares := real.Middlewares
	toolMiddlewares := agents.ToolCallMiddlewaresOf(middlewares)

	var historyOptions []history.ConversationManagerOptions
	if real.History.Summarizer != nil {
		historyOptions = append(historyOptions, history.WithSummarizer(restate_runtime.NewRestateConversationSummarizer(ctx, real.History.Summarizer)))
	}
	if real.History.MessageFilter != nil {
		historyOptions = append(historyOptions, history.WithMessageFilter(restate_runtime.NewRestateMessageFilter(ctx, real.History.MessageFilter)))
	}
	persistence := restate_runtime.NewRestateConversationPersistence(ctx, real.History.ConversationPersistenceAdapter, agents.HistoryMiddlewaresOf(middlewares)...)

	// AgentOptions lists the catalog's tools in the config's order, so each
	// keeps its catalog key, which a background task's service needs.
	tools := make([]agents.Tool, 0, len(real.Tools))
	for i, tool := range real.Tools {
		proxy := restate_runtime.NewRestateTool(ctx, tool, b.broker, toolMiddlewares...)
		if _, ok := tool.(agents.BackgroundTool); ok {
			tools = append(tools, &backgroundTool{RestateTool: proxy, agent: cfg.Name, key: cfg.Tools[i]})
			continue
		}
		tools = append(tools, proxy)
	}
	// A sub-agent call runs as a step: the sub-agent's own run, which on this
	// runtime is an invocation of its own the step waits for.
	for _, tool := range b.builder.SubAgentTools(input.Snapshot, name) {
		tools = append(tools, restate_runtime.NewRestateTool(ctx, tool, b.broker, toolMiddlewares...))
	}
	var mcpClient agents.MCPClient
	if real.MCPClient != nil {
		mcpClient = restate_runtime.NewRestateMCPClient(ctx, real.MCPClient, b.broker, toolMiddlewares...)
	}
	var skillClient agents.SkillClient
	if real.SkillClient != nil {
		skillClient = restate_runtime.NewRestateSkillClient(ctx, real.SkillClient, b.broker, toolMiddlewares...)
	}

	options := &agents.AgentOptions{
		Name:          real.Name,
		Output:        real.Output,
		Parameters:    real.Parameters,
		MaxLoops:      real.MaxLoops,
		StickyHandoff: real.StickyHandoff,

		History:      history.NewConversationManager(persistence, historyOptions...),
		Instruction:  restate_runtime.NewRestatePrompt(ctx, real.Instruction, agents.PromptMiddlewaresOf(middlewares)...),
		Tools:        tools,
		SkillClient:  skillClient,
		MCPClient:    mcpClient,
		ClientTools:  real.ClientTools,
		ToolExecutor: restate_runtime.NewRestateToolExecutor(ctx),
		StreamBroker: restate_runtime.NewRestateStreamBroker(ctx, b.broker),
		DurableStep:  restate_runtime.NewRestateDurableStep(ctx),
		// A task's wait outlives this run, so it goes to an invocation of the
		// background service, carrying the snapshot; see background.go.
		BackgroundRunner: &backgroundRunner{ctx: ctx, snapshot: input.Snapshot, providerConfigKey: input.ProviderConfigKey},
	}
	llm := restate_runtime.NewRestateLLM(ctx, real.LLM, input.ProviderConfigKey, b.broker, streamID, agents.ModelCallMiddlewaresOf(middlewares)...)
	agent := agents.NewAgent(options).WithLLM(llm)
	built[name] = agent

	for _, h := range cfg.Handoffs {
		if _, ok := input.Snapshot.Agents[h.Name]; !ok {
			slog.Warn("handoff target is not in the agent snapshot; the edge will not exist in this run",
				slog.String("agent", cfg.Name), slog.String("target", h.Name))
			continue
		}
		target, err := b.buildAgent(ctx, input, streamID, h.Name, built)
		if err != nil {
			return nil, err
		}
		agent.AddHandoffs(agents.NewHandoff(h.Name, h.Description, target))
	}
	return agent, nil
}
