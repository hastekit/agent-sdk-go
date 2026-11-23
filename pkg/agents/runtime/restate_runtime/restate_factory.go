package restate_runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/server"
)

const (
	// FactoryWorkflowServiceName is the one workflow service every factory-built agent runs on.
	FactoryWorkflowServiceName = "FactoryAgentWorkflow"
	// FactoryBackgroundTaskServiceName waits on background tasks factory agents start.
	FactoryBackgroundTaskServiceName = "FactoryBackgroundTaskService"
)

// FactoryWorkflowInput is a run's input plus the ref it is pinned to.
type FactoryWorkflowInput struct {
	Ref agents.AgentRef `json:"ref"`
	WorkflowInput
}

// BindFactory binds the factory workflow and its background service to srv.
func BindFactory(srv *server.Restate, factory agents.AgentFactory, broker agents.StreamBroker) *server.Restate {
	cached := agents.NewCachedAgentFactory(factory, 0)
	return srv.
		Bind(restate.Reflect(NewFactoryAgentWorkflow(cached, broker))).
		Bind(restate.Reflect(NewFactoryBackgroundTaskService(cached, broker)))
}

// FactoryAgentWorkflow runs factory-built agents by ref. Its shape comes from a journaled
// DescribeAgent step, and each step rebuilds the real component from the factory, cached by ref.
type FactoryAgentWorkflow struct {
	factory agents.AgentFactory
	broker  agents.StreamBroker
}

func NewFactoryAgentWorkflow(factory agents.AgentFactory, broker agents.StreamBroker) *FactoryAgentWorkflow {
	return &FactoryAgentWorkflow{factory: agents.NewCachedAgentFactory(factory, 0), broker: broker}
}

// Run executes input.Ref inside a Restate workflow context.
func (w *FactoryAgentWorkflow) Run(restateCtx restate.WorkflowContext, input *FactoryWorkflowInput) (*agents.AgentOutput, error) {
	streamID := input.StreamID
	if streamID == "" {
		streamID = restate.Key(restateCtx)
	}
	restateCtx = withGatewayContext(restateCtx, input.ProviderConfigKey, input.GatewayContext)

	specs, err := restate.Run(restateCtx, func(runCtx restate.RunContext) ([]agents.AgentSpec, error) {
		return describeFactoryAgent(runCtx, w.factory, input.Ref)
	}, restate.WithName("DescribeAgent"))
	if err != nil {
		return &agents.AgentOutput{Status: agentstate.RunStatusError}, err
	}
	if len(specs) == 0 {
		return &agents.AgentOutput{Status: agentstate.RunStatusError}, restate.TerminalError(fmt.Errorf("agent %s described nothing", input.Ref))
	}

	agent := w.proxyGraph(restateCtx, specs, input.ProviderConfigKey, streamID)
	return agent.ExecuteWithoutTrace(restateCtx, &agents.AgentInput{
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

// describeFactoryAgent describes ref's graph; an unknown ref or missing history is terminal.
func describeFactoryAgent(ctx context.Context, factory agents.AgentFactory, ref agents.AgentRef) ([]agents.AgentSpec, error) {
	specs, err := agents.DescribeAgentGraph(ctx, factory, ref)
	if err != nil {
		return nil, factoryError(err)
	}
	for _, spec := range specs {
		options, err := agents.BuildFactoryOptions(ctx, factory, spec.Ref)
		if err != nil {
			return nil, factoryError(err)
		}
		if options.History == nil {
			return nil, configError("agent %s has no History; durable runs need persistent history", spec.Ref)
		}
	}
	return specs, nil
}

func factoryError(err error) error {
	if errors.Is(err, agents.ErrAgentNotFound) {
		return restate.TerminalError(err, http.StatusNotFound)
	}
	return err
}

func configError(format string, args ...any) error {
	return restate.TerminalError(fmt.Errorf(format, args...), http.StatusUnprocessableEntity)
}

// proxyGraph builds every agent before wiring edges, so handoff cycles resolve to the same proxies.
func (w *FactoryAgentWorkflow) proxyGraph(restateCtx restate.WorkflowContext, specs []agents.AgentSpec, providerConfigKey, streamID string) *agents.Agent {
	built := make(map[agents.AgentRef]*agents.Agent, len(specs))
	for i := range specs {
		if _, ok := built[specs[i].Ref]; !ok {
			built[specs[i].Ref] = w.proxyAgent(restateCtx, &specs[i], providerConfigKey, streamID)
		}
	}
	for _, spec := range specs {
		agent := built[spec.Ref]
		for _, h := range spec.Handoffs {
			if target := built[h.Ref]; target != nil {
				agent.AddHandoffs(agents.NewHandoff(h.Ref.Name, h.Description, target))
			}
		}
	}
	return built[specs[0].Ref]
}

func (w *FactoryAgentWorkflow) proxyAgent(restateCtx restate.WorkflowContext, spec *agents.AgentSpec, providerConfigKey, streamID string) *agents.Agent {
	b := &factoryBinding{ctx: restateCtx, factory: w.factory, ref: spec.Ref, broker: w.broker}

	var historyOptions []history.ConversationManagerOptions
	if spec.HasSummarizer {
		historyOptions = append(historyOptions, history.WithSummarizer(&factorySummarizer{b}))
	}
	if spec.HasMessageFilter {
		historyOptions = append(historyOptions, history.WithMessageFilter(&factoryFilter{b}))
	}
	persistence := &factoryHistory{RestateHistory: NewRestateConversationPersistence(restateCtx, nil), b: b}
	conversationHistory := history.NewConversationManager(persistence, historyOptions...)
	conversationHistory.MessageAttribution = spec.MessageAttribution
	conversationHistory.SteeringNotices = spec.SteeringNotices

	var instruction agents.SystemPromptProvider
	if spec.HasInstruction {
		instruction = &factoryPrompt{b}
	}

	tools := make([]agents.Tool, 0, len(spec.Tools))
	for _, toolSpec := range spec.Tools {
		tool := &factoryTool{b: b, spec: toolSpec}
		if toolSpec.Background {
			tools = append(tools, &factoryBackgroundTool{factoryTool: tool})
		} else {
			tools = append(tools, tool)
		}
	}

	var mcpClient agents.MCPClient
	if spec.HasMCP {
		mcpClient = &factoryMCP{b}
	}
	var skillClient agents.SkillClient
	if spec.HasSkills {
		skillClient = &factorySkills{b}
	}

	opts := &agents.AgentOptions{
		Name:          spec.Ref.Name,
		Version:       spec.Ref.Version,
		Output:        spec.Output,
		Parameters:    spec.Parameters,
		MaxLoops:      spec.MaxLoops,
		StickyHandoff: spec.StickyHandoff,
		SingleTurn:    spec.SingleTurn,
		Instruction:   instruction,
		History:       conversationHistory,
		Tools:         tools,
		SkillClient:   skillClient,
		MCPClient:     mcpClient,
		ClientTools:   spec.ClientTools,
		ToolExecutor:  NewRestateToolExecutor(restateCtx),
		StreamBroker:  NewRestateStreamBroker(restateCtx, w.broker),
		DurableStep:   NewRestateDurableStep(restateCtx),
		BackgroundRunner: &factoryBackgroundRunner{
			restateCtx: restateCtx, owner: spec.Ref, providerConfigKey: providerConfigKey,
		},
	}
	return agents.NewAgent(opts).WithLLM(&factoryLLM{b: b, providerConfigKey: providerConfigKey, streamID: streamID})
}

// factoryBinding resolves one ref's real components inside a step.
type factoryBinding struct {
	ctx     restate.WorkflowContext
	factory agents.AgentFactory
	ref     agents.AgentRef
	broker  agents.StreamBroker
}

func (b *factoryBinding) options(ctx context.Context) (*agents.AgentOptions, error) {
	options, err := agents.BuildFactoryOptions(ctx, b.factory, b.ref)
	if err != nil {
		return nil, factoryError(err)
	}
	return options, nil
}

func (b *factoryBinding) history(ctx context.Context) (*agents.AgentOptions, error) {
	options, err := b.options(ctx)
	if err != nil {
		return nil, err
	}
	if options.History == nil {
		return nil, configError("agent %s has no History", b.ref)
	}
	return options, nil
}

func (b *factoryBinding) toolCallMiddlewares(options *agents.AgentOptions) []agents.ToolCallMiddleware {
	return agents.ToolCallMiddlewaresOf(options.Middlewares)
}

type factoryPrompt struct{ b *factoryBinding }

func (p *factoryPrompt) GetPrompt(_ context.Context, deps *agents.Dependencies) (string, error) {
	return restate.Run(p.b.ctx, func(ctx restate.RunContext) (string, error) {
		options, err := p.b.options(ctx)
		if err != nil {
			return "", err
		}
		if options.Instruction == nil {
			return "", configError("agent %s has no instruction", p.b.ref)
		}
		return agents.WrapPromptProvider(options.Instruction, agents.PromptMiddlewaresOf(options.Middlewares)...).GetPrompt(ctx, deps)
	}, restate.WithName("GetPrompt"))
}

type factoryLLM struct {
	b                 *factoryBinding
	providerConfigKey string
	streamID          string
}

func (l *factoryLLM) NewStreamingResponses(_ context.Context, call *agents.ModelCall, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	resp, err := restate.Run(l.b.ctx, func(ctx restate.RunContext) (*responses.Response, error) {
		options, err := l.b.options(ctx)
		if err != nil {
			return nil, err
		}
		inner := NewRestateLLM(nil, options.LLM, l.providerConfigKey, l.b.broker, l.streamID, agents.ModelCallMiddlewaresOf(options.Middlewares)...)
		return inner.(*RestateLLM).invoke(ctx, call, in, cb)
	}, restate.WithName("LLMCall"))
	if err != nil && wasCancelled(err) {
		return nil, agents.ErrModelCallStopped
	}
	return resp, err
}

// factoryHistory keeps RestateHistory's journaled ids and clock, and resolves persistence per step.
type factoryHistory struct {
	*RestateHistory
	b *factoryBinding
}

func (h *factoryHistory) persistence(ctx context.Context) (history.ConversationPersistenceAdapter, error) {
	options, err := h.b.history(ctx)
	if err != nil {
		return nil, err
	}
	return agents.WrapHistoryPersistence(options.History.ConversationPersistenceAdapter, agents.HistoryMiddlewaresOf(options.Middlewares)...), nil
}

func (h *factoryHistory) LoadMessages(_ context.Context, namespace, threadID, previousRunID string) ([]history.ConversationMessage, error) {
	return restate.Run(h.b.ctx, func(ctx restate.RunContext) ([]history.ConversationMessage, error) {
		p, err := h.persistence(ctx)
		if err != nil {
			return nil, err
		}
		return p.LoadMessages(ctx, namespace, threadID, previousRunID)
	}, restate.WithName("LoadMessages"))
}

func (h *factoryHistory) SaveMessages(_ context.Context, namespace, groupID, runID, previousRunID, threadID, conversationID string, msgs []history.Message, meta map[string]any) error {
	_, err := restate.Run(h.b.ctx, func(ctx restate.RunContext) (any, error) {
		p, err := h.persistence(ctx)
		if err != nil {
			return nil, err
		}
		return nil, p.SaveMessages(ctx, namespace, groupID, runID, previousRunID, threadID, conversationID, msgs, meta)
	}, restate.WithName("SaveMessages"))
	return err
}

func (h *factoryHistory) SaveSummary(_ context.Context, namespace string, summary history.Summary) error {
	_, err := restate.Run(h.b.ctx, func(ctx restate.RunContext) (any, error) {
		p, err := h.persistence(ctx)
		if err != nil {
			return nil, err
		}
		return nil, p.SaveSummary(ctx, namespace, summary)
	}, restate.WithName("SaveSummary"))
	return err
}

// factorySummarizer journals both the decision and the summary, since the summarizer lives in the factory.
type factorySummarizer struct{ b *factoryBinding }

func (s *factorySummarizer) summarizer(ctx context.Context) (history.HistorySummarizer, error) {
	options, err := s.b.history(ctx)
	if err != nil {
		return nil, err
	}
	if options.History.Summarizer == nil {
		return nil, configError("agent %s has no summarizer", s.b.ref)
	}
	return options.History.Summarizer, nil
}

func (s *factorySummarizer) ShouldSummarize(_ context.Context, ids map[string]string, msgs []messages.Message, tokens int) (bool, error) {
	return restate.Run(s.b.ctx, func(ctx restate.RunContext) (bool, error) {
		summarizer, err := s.summarizer(ctx)
		if err != nil {
			return false, err
		}
		return summarizer.ShouldSummarize(ctx, ids, msgs, tokens)
	}, restate.WithName("ShouldSummarize"))
}

func (s *factorySummarizer) Summarize(_ context.Context, ids map[string]string, msgs []messages.Message, tokens int) (*history.SummaryResult, error) {
	return restate.Run(s.b.ctx, func(ctx restate.RunContext) (*history.SummaryResult, error) {
		summarizer, err := s.summarizer(ctx)
		if err != nil {
			return nil, err
		}
		return summarizer.Summarize(ctx, ids, msgs, tokens)
	}, restate.WithName("Summarize"))
}

type factoryFilter struct{ b *factoryBinding }

func (f *factoryFilter) Filter(_ context.Context, msgs []messages.Message, agentID string) []messages.Message {
	out, err := restate.Run(f.b.ctx, func(ctx restate.RunContext) ([]messages.Message, error) {
		options, err := f.b.history(ctx)
		if err != nil {
			return nil, err
		}
		if options.History.MessageFilter == nil {
			return msgs, nil
		}
		return options.History.MessageFilter.Filter(ctx, msgs, agentID), nil
	}, restate.WithName("FilterMessages"))
	if err != nil {
		// Best-effort, as for registered agents: keep the unfiltered bundles.
		return msgs
	}
	return out
}

// factoryTool is known in the workflow by its descriptor and resolved by key inside its step.
type factoryTool struct {
	b    *factoryBinding
	spec agents.ToolSpec
}

func (t *factoryTool) GetToolDescriptor() *agents.BaseTool { return &t.spec.Tool }

func (t *factoryTool) Execute(_ context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return restate.Run(t.b.ctx, func(ctx restate.RunContext) (*agents.ToolCallResponse, error) {
		options, tool, err := t.resolve(ctx)
		if err != nil {
			return nil, err
		}
		return NewRestateTool(nil, tool, t.b.broker, t.b.toolCallMiddlewares(options)...).execute(ctx, call)
	}, restate.WithName(call.Name+"_ToolCall"))
}

func (t *factoryTool) resolve(ctx context.Context) (*agents.AgentOptions, agents.Tool, error) {
	return resolveFactoryTool(ctx, t.b.factory, t.b.ref, t.spec.Key())
}

func resolveFactoryTool(ctx context.Context, factory agents.AgentFactory, ref agents.AgentRef, key string) (*agents.AgentOptions, agents.Tool, error) {
	options, err := agents.BuildFactoryOptions(ctx, factory, ref)
	if err != nil {
		return nil, nil, factoryError(err)
	}
	tool := agents.FindToolByKey(options.Tools, key)
	if tool == nil {
		return nil, nil, configError("agent %s has no tool %q", ref, key)
	}
	return options, tool, nil
}

// factoryBackgroundTool keeps the background capability the loop asks about; its wait runs in the service.
type factoryBackgroundTool struct{ *factoryTool }

var _ agents.BackgroundTool = (*factoryBackgroundTool)(nil)

func (t *factoryBackgroundTool) AwaitTask(context.Context, agents.BackgroundTaskRef, agents.ProgressReporter) (agents.BackgroundResult, error) {
	return agents.BackgroundResult{}, fmt.Errorf(
		"a background task is waited on by its own invocation, not inside the run that started it")
}

type factoryMCP struct{ b *factoryBinding }

func (c *factoryMCP) client(ctx context.Context) (*RestateMCPClient, error) {
	options, err := c.b.options(ctx)
	if err != nil {
		return nil, err
	}
	if options.MCPClient == nil {
		return nil, configError("agent %s has no MCP client", c.b.ref)
	}
	return NewRestateMCPClient(nil, options.MCPClient, c.b.broker, c.b.toolCallMiddlewares(options)...), nil
}

func (c *factoryMCP) ListTools(_ context.Context, namespace string, runContext map[string]any, selection ...agents.MCPSelection) ([]agents.ConnectorStatus, []agents.Tool, error) {
	if len(selection) > 1 {
		return nil, nil, errors.New("MCP listing accepts at most one selection")
	}
	result, err := restate.Run(c.b.ctx, func(ctx restate.RunContext) (*agents.MCPListing, error) {
		client, err := c.client(ctx)
		if err != nil {
			return nil, err
		}
		return client.listTools(ctx, namespace, runContext, selection...)
	}, restate.WithName("MCPListTools"))
	if err != nil {
		return nil, nil, err
	}
	tools := make([]agents.Tool, 0, len(result.Tools))
	for _, tool := range result.Tools {
		tools = append(tools, &factoryMCPTool{BaseTool: &tool, client: c})
	}
	return result.Connectors, tools, nil
}

func (c *factoryMCP) CallTool(_ context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return restate.Run(c.b.ctx, func(ctx restate.RunContext) (*agents.ToolCallResponse, error) {
		client, err := c.client(ctx)
		if err != nil {
			return nil, err
		}
		return client.executeTool(ctx, tool, call)
	}, restate.WithName("MCPToolCall"))
}

type factoryMCPTool struct {
	*agents.BaseTool
	client *factoryMCP
}

func (t *factoryMCPTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return t.client.CallTool(ctx, t.BaseTool, call)
}

type factorySkills struct{ b *factoryBinding }

var (
	_ agents.SkillClient       = (*factorySkills)(nil)
	_ agents.SkillReadExecutor = (*factorySkills)(nil)
)

func (s *factorySkills) client(ctx context.Context) (*RestateSkillClient, error) {
	options, err := s.b.options(ctx)
	if err != nil {
		return nil, err
	}
	if options.SkillClient == nil {
		return nil, configError("agent %s has no skill client", s.b.ref)
	}
	return NewRestateSkillClient(nil, options.SkillClient, s.b.broker, s.b.toolCallMiddlewares(options)...), nil
}

func (s *factorySkills) ListSkills(_ context.Context, namespace string, rc map[string]any) ([]agents.Skill, error) {
	return restate.Run(s.b.ctx, func(ctx restate.RunContext) ([]agents.Skill, error) {
		client, err := s.client(ctx)
		if err != nil {
			return nil, err
		}
		return client.client.ListSkills(ctx, namespace, rc)
	}, restate.WithName("ListSkills"))
}

func (s *factorySkills) ReadSkill(_ context.Context, namespace string, rc map[string]any, name, file string) (string, error) {
	return restate.Run(s.b.ctx, func(ctx restate.RunContext) (string, error) {
		client, err := s.client(ctx)
		if err != nil {
			return "", err
		}
		return client.client.ReadSkill(ctx, namespace, rc, name, file)
	}, restate.WithName("ReadSkill"))
}

func (s *factorySkills) ReadSkillCall(_ context.Context, namespace string, rc map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	return restate.Run(s.b.ctx, func(ctx restate.RunContext) (string, error) {
		client, err := s.client(ctx)
		if err != nil {
			return "", err
		}
		content, err := agents.ReadSkillWithMiddleware(ctx, client.client, namespace, rc, name, file, call, client.middlewares)
		return content, cancellationError(err)
	}, restate.WithName("ReadSkillCall"))
}

// FactoryBackgroundTaskInput names who owns the thread and which agent's tool waits.
type FactoryBackgroundTaskInput struct {
	Owner             agents.AgentRef          `json:"owner"`
	ToolAgent         agents.AgentRef          `json:"tool_agent"`
	ToolKey           string                   `json:"tool_key"`
	Ref               agents.BackgroundTaskRef `json:"ref"`
	ProviderConfigKey string                   `json:"provider_config_key,omitempty"`
	GatewayContext    map[string]string        `json:"gateway_context,omitempty"`
}

// factoryBackgroundRunner hands a task's wait to the factory background service, pinned to the owner's ref.
type factoryBackgroundRunner struct {
	restateCtx        restate.WorkflowContext
	owner             agents.AgentRef
	providerConfigKey string
}

func (r *factoryBackgroundRunner) StartTask(_ context.Context, tool agents.BackgroundTool, ref agents.BackgroundTaskRef) error {
	proxy, ok := tool.(*factoryBackgroundTool)
	if !ok {
		return fmt.Errorf("background tool for task %s is not a factory workflow proxy", ref.TaskID)
	}
	restate.ServiceSend(r.restateCtx, FactoryBackgroundTaskServiceName, "Await").Send(&FactoryBackgroundTaskInput{
		Owner:             r.owner,
		ToolAgent:         proxy.b.ref,
		ToolKey:           proxy.spec.Key(),
		Ref:               ref,
		ProviderConfigKey: r.providerConfigKey,
		GatewayContext:    gatewayContextOf(r.restateCtx),
	})
	return nil
}

// FactoryBackgroundTaskService waits for factory agents' background tasks and delivers their results.
type FactoryBackgroundTaskService struct {
	factory agents.AgentFactory
	broker  agents.StreamBroker
}

func NewFactoryBackgroundTaskService(factory agents.AgentFactory, broker agents.StreamBroker) *FactoryBackgroundTaskService {
	return &FactoryBackgroundTaskService{factory: agents.NewCachedAgentFactory(factory, 0), broker: broker}
}

// Await waits for one task, then puts its outcome in front of the owner at its pinned ref.
func (s *FactoryBackgroundTaskService) Await(ctx restate.Context, in *FactoryBackgroundTaskInput) (restate.Void, error) {
	ctx = withGatewayContext(ctx, in.ProviderConfigKey, in.GatewayContext)
	return runBackgroundTask(ctx, s.broker, in.ToolAgent.String()+"/"+in.ToolKey, in.Ref,
		func(runCtx restate.RunContext, progress agents.ProgressReporter) (*awaitOutcome, error) {
			options, tool, err := resolveFactoryTool(runCtx, s.factory, in.ToolAgent, in.ToolKey)
			if err != nil {
				return nil, err
			}
			background, ok := tool.(agents.BackgroundTool)
			if !ok {
				return nil, configError("tool %q of agent %s cannot wait for tasks", in.ToolKey, in.ToolAgent)
			}
			wrapped := agents.WrapBackgroundTool(in.ToolAgent.Name, background, agents.ToolCallMiddlewaresOf(options.Middlewares)...)
			return outcomeOf(wrapped.AwaitTask(runCtx, in.Ref, progress)), nil
		},
		func(decision *DeliverTaskOutput) {
			restate.WorkflowSend(ctx, FactoryWorkflowServiceName, in.Ref.ThreadStreamID, "Run").Send(&FactoryWorkflowInput{
				Ref: in.Owner,
				WorkflowInput: WorkflowInput{
					AgentName:         in.Owner.Name,
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
					GatewayContext:    in.GatewayContext,
				},
			})
		})
}
