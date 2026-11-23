package temporal_runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	// FactoryAgentWorkflowName is the one workflow type every factory-built agent runs on, started with (ref, input).
	FactoryAgentWorkflowName = "hastekit_FactoryAgentWorkflow"
	// FactoryBackgroundTaskWorkflowName waits on a background task a factory agent started.
	FactoryBackgroundTaskWorkflowName = "hastekit_FactoryBackgroundTaskWorkflow"
	// AgentNotFoundErrorType marks a ref the factory does not know; it is not retried.
	AgentNotFoundErrorType = "AgentNotFound"
	// AgentConfigErrorType marks a built agent that lacks what a step needs; it is not retried.
	AgentConfigErrorType = "AgentConfig"

	factoryActivityPrefix = "hastekit_Factory"
	describeAgentActivity = factoryActivityPrefix + "_DescribeAgentActivity"
)

// executeActivity schedules name, putting ref first when the proxy belongs to a factory agent.
func executeActivity(ctx workflow.Context, ref *agents.AgentRef, name string, args ...any) workflow.Future {
	if ref != nil {
		args = append([]any{*ref}, args...)
	}
	return workflow.ExecuteActivity(ctx, name, args...)
}

// shouldSummarizeLocally asks the worker's summarizer through a local activity, journaling only the answer.
func shouldSummarizeLocally(ctx workflow.Context, decide any, ref agents.AgentRef, ids map[string]string, msgs []messages.Message, tokens int) (bool, error) {
	ctx = workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{StartToCloseTimeout: 30 * time.Second})
	var decision bool
	err := workflow.ExecuteLocalActivity(ctx, decide, ref, ids, msgs, tokens).Get(ctx, &decision)
	return decision, err
}

// FactoryAgentWorkflow runs factory-built agents by ref, so agents created after the worker
// starts need no registration. Activities rebuild the agent from the factory, cached by ref.
type FactoryAgentWorkflow struct {
	factory agents.AgentFactory
	broker  agents.StreamBroker
}

func NewFactoryAgentWorkflow(factory agents.AgentFactory, broker agents.StreamBroker) *FactoryAgentWorkflow {
	return &FactoryAgentWorkflow{factory: agents.NewCachedAgentFactory(factory, 0), broker: broker}
}

// GetWorkflows returns the factory workflows to register, by name.
func (f *FactoryAgentWorkflow) GetWorkflows() map[string]any {
	return map[string]any{
		FactoryAgentWorkflowName:          f.Execute,
		FactoryBackgroundTaskWorkflowName: f.ExecuteBackgroundTask,
	}
}

// GetActivities returns the shared activities; each takes the agent's ref first.
func (f *FactoryAgentWorkflow) GetActivities() map[string]any {
	p := factoryActivityPrefix
	return map[string]any{
		describeAgentActivity:                f.DescribeAgent,
		p + heartbeatActivitySuffix:          f.StreamHeartbeat,
		p + "_GetPromptActivity":             f.GetPrompt,
		p + "_NewStreamingResponsesActivity": f.NewStreamingResponses,
		p + "_LoadMessagesActivity":          f.LoadMessages,
		p + "_SaveMessagesActivity":          f.SaveMessages,
		p + "_SaveSummaryActivity":           f.SaveSummary,
		p + "_SummarizerActivity":            f.Summarize,
		p + "_MessageFilterActivity":         f.FilterMessages,
		p + "_IsStoppedActivity":             f.IsStopped,
		p + "_DrainMessagesActivity":         f.DrainMessages,
		p + "_WaitToolResultActivity":        f.WaitToolResult,
		p + "_ExecuteToolActivity":           f.ExecuteTool,
		p + awaitTaskActivitySuffix:          f.AwaitTask,
		p + closeTaskStreamActivityName:      f.CloseTaskStream,
		p + deliverTaskActivityName:          f.DeliverTask,
		p + "_MCP_ListMCPToolsActivity":      f.ListMCPTools,
		p + "_MCP_ExecuteMCPToolActivity":    f.ExecuteMCPTool,
		p + "_Skills_ListSkills":             f.ListSkills,
		p + "_Skills_ReadSkill":              f.ReadSkill,
	}
}

// Execute runs ref. The agent's shape comes from DescribeAgent's journaled result, so replay
// rebuilds the same graph even if the stored configuration has since changed.
func (f *FactoryAgentWorkflow) Execute(ctx workflow.Context, ref agents.AgentRef, in *agents.AgentInput) (*agents.AgentOutput, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Second})
	if in.StreamID == "" {
		in.StreamID = workflow.GetInfo(ctx).WorkflowExecution.ID
	}

	var specs []agents.AgentSpec
	if err := workflow.ExecuteActivity(ctx, describeAgentActivity, ref).Get(ctx, &specs); err != nil {
		return &agents.AgentOutput{Status: agentstate.RunStatusError}, err
	}
	if len(specs) == 0 {
		return &agents.AgentOutput{Status: agentstate.RunStatusError}, fmt.Errorf("agent %s described nothing", ref)
	}
	agent := f.proxyGraph(ctx, specs)

	// Bridge the workflow span into the loop's context, as the registered workflow does.
	goCtx := context.Background()
	if span, ok := opentelemetry.SpanFromWorkflowContext(ctx); ok {
		goCtx = trace.ContextWithSpan(goCtx, span)
	}
	return agent.ExecuteWithoutTrace(goCtx, in)
}

// proxyGraph builds every agent before wiring edges, so handoff cycles resolve to the same proxies.
func (f *FactoryAgentWorkflow) proxyGraph(ctx workflow.Context, specs []agents.AgentSpec) *agents.Agent {
	built := make(map[agents.AgentRef]*agents.Agent, len(specs))
	for i := range specs {
		if _, ok := built[specs[i].Ref]; !ok {
			built[specs[i].Ref] = f.proxyAgent(ctx, &specs[i])
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

// proxyAgent is the workflow-side agent for one spec; every real component runs in an activity.
func (f *FactoryAgentWorkflow) proxyAgent(ctx workflow.Context, spec *agents.AgentSpec) *agents.Agent {
	ref := spec.Ref
	p := factoryActivityPrefix

	var historyOptions []history.ConversationManagerOptions
	if spec.HasSummarizer {
		historyOptions = append(historyOptions, history.WithSummarizer(&TemporalConversationSummarizerProxy{
			workflowCtx: ctx, prefix: p, ref: &ref, decide: f.ShouldSummarize,
		}))
	}
	if spec.HasMessageFilter {
		historyOptions = append(historyOptions, history.WithMessageFilter(&TemporalMessageFilterProxy{workflowCtx: ctx, prefix: p, ref: &ref}))
	}
	conversationHistory := history.NewConversationManager(&TemporalConversationPersistenceProxy{workflowCtx: ctx, prefix: p, ref: &ref}, historyOptions...)
	conversationHistory.MessageAttribution = spec.MessageAttribution
	conversationHistory.SteeringNotices = spec.SteeringNotices

	var instruction agents.SystemPromptProvider
	if spec.HasInstruction {
		instruction = &TemporalPromptProxy{workflowCtx: ctx, prefix: p, ref: &ref}
	}

	tools := make([]agents.Tool, 0, len(spec.Tools))
	for _, toolSpec := range spec.Tools {
		tools = append(tools, newFactoryToolProxy(ctx, ref, toolSpec))
	}

	var mcpClient agents.MCPClient
	if spec.HasMCP {
		mcpClient = &temporalMCPClientProxy{ctx: ctx, prefix: p + "_MCP", ref: &ref}
	}
	var skillClient agents.SkillClient
	if spec.HasSkills {
		skillClient = &temporalSkillClientProxy{ctx: ctx, prefix: p + "_Skills", ref: &ref}
	}

	opts := &agents.AgentOptions{
		Name:             ref.Name,
		Version:          ref.Version,
		Output:           spec.Output,
		Parameters:       spec.Parameters,
		MaxLoops:         spec.MaxLoops,
		StickyHandoff:    spec.StickyHandoff,
		SingleTurn:       spec.SingleTurn,
		History:          conversationHistory,
		Instruction:      instruction,
		Tools:            tools,
		SkillClient:      skillClient,
		MCPClient:        mcpClient,
		ClientTools:      spec.ClientTools,
		ToolExecutor:     NewTemporalToolExecutor(ctx),
		StreamBroker:     &TemporalStreamBrokerProxy{workflowCtx: ctx, prefix: p, ref: &ref, wrappedBroker: f.broker},
		DurableStep:      NewTemporalDurableStep(ctx),
		BackgroundRunner: &factoryBackgroundRunner{workflowCtx: ctx, owner: ref},
	}
	return agents.NewAgent(opts).WithLLM(&TemporalLLMProxy{workflowCtx: ctx, prefix: p, ref: &ref, broker: f.broker})
}

// specTool is a tool known in the workflow only by its descriptor.
type specTool struct{ *agents.BaseTool }

func (specTool) Execute(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return nil, errors.New("a factory agent's tool runs in its activity, not in the workflow")
}

func newFactoryToolProxy(ctx workflow.Context, ref agents.AgentRef, spec agents.ToolSpec) agents.Tool {
	descriptor := spec.Tool
	proxy := &TemporalToolProxy{
		workflowCtx: ctx,
		prefix:      factoryActivityPrefix,
		wrappedTool: specTool{BaseTool: &descriptor},
		ref:         &ref,
		toolKey:     spec.Key(),
	}
	if spec.Background {
		return &TemporalBackgroundToolProxy{TemporalToolProxy: proxy}
	}
	return proxy
}

// FactoryBackgroundTaskInput names who owns the thread and which agent's tool waits.
type FactoryBackgroundTaskInput struct {
	Owner     agents.AgentRef          `json:"owner"`
	ToolAgent agents.AgentRef          `json:"tool_agent"`
	ToolKey   string                   `json:"tool_key"`
	Ref       agents.BackgroundTaskRef `json:"ref"`
}

// factoryBackgroundRunner starts a task's wait as a child workflow pinned to the owner's ref.
type factoryBackgroundRunner struct {
	workflowCtx workflow.Context
	owner       agents.AgentRef
}

func (r *factoryBackgroundRunner) StartTask(_ context.Context, tool agents.BackgroundTool, ref agents.BackgroundTaskRef) error {
	proxy, ok := tool.(*TemporalBackgroundToolProxy)
	if !ok || proxy.ref == nil {
		return fmt.Errorf("background tool for task %s is not a factory workflow proxy", ref.TaskID)
	}
	ctx := workflow.WithChildOptions(r.workflowCtx, workflow.ChildWorkflowOptions{
		WorkflowID:            backgroundWorkflowID(r.owner.Name, ref),
		ParentClosePolicy:     enums.PARENT_CLOSE_POLICY_ABANDON,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	})
	child := workflow.ExecuteChildWorkflow(ctx, FactoryBackgroundTaskWorkflowName, &FactoryBackgroundTaskInput{
		Owner:     r.owner,
		ToolAgent: *proxy.ref,
		ToolKey:   proxy.toolKey,
		Ref:       ref,
	})
	// Wait for the start, not the finish, so the child exists before this run completes.
	return child.GetChildWorkflowExecution().Get(ctx, nil)
}

// ExecuteBackgroundTask waits for one task and, if the thread is idle, runs the owner at its pinned ref.
func (f *FactoryAgentWorkflow) ExecuteBackgroundTask(ctx workflow.Context, in *FactoryBackgroundTaskInput) error {
	return runBackgroundTask(ctx, f.broker, backgroundTaskSteps{
		prefix: factoryActivityPrefix,
		ref:    &in.Owner,
		await: func(ctx workflow.Context) workflow.Future {
			return executeActivity(ctx, &in.ToolAgent, factoryActivityPrefix+awaitTaskActivitySuffix, in.ToolKey, in.Ref)
		},
		run: func(ctx workflow.Context, input *agents.AgentInput) workflow.ChildWorkflowFuture {
			return workflow.ExecuteChildWorkflow(ctx, FactoryAgentWorkflowName, in.Owner, input)
		},
	}, in.Ref)
}

// options rebuilds ref on the worker; an unknown ref fails without retries.
func (f *FactoryAgentWorkflow) options(ctx context.Context, ref agents.AgentRef) (*agents.AgentOptions, error) {
	options, err := agents.BuildFactoryOptions(ctx, f.factory, ref)
	if err != nil {
		return nil, factoryError(err)
	}
	return options, nil
}

func configError(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), AgentConfigErrorType, nil)
}

func factoryError(err error) error {
	if errors.Is(err, agents.ErrAgentNotFound) {
		return temporal.NewNonRetryableApplicationError(err.Error(), AgentNotFoundErrorType, err)
	}
	return err
}

// DescribeAgent returns the specs for ref and every handoff target it reaches, ref first.
func (f *FactoryAgentWorkflow) DescribeAgent(ctx context.Context, ref agents.AgentRef) ([]agents.AgentSpec, error) {
	specs, err := agents.DescribeAgentGraph(ctx, f.factory, ref)
	if err != nil {
		return nil, factoryError(err)
	}
	for _, spec := range specs {
		options, err := f.options(ctx, spec.Ref)
		if err != nil {
			return nil, err
		}
		if options.History == nil {
			return nil, configError("agent %s has no History; durable runs need persistent history", spec.Ref)
		}
	}
	return specs, nil
}

func (f *FactoryAgentWorkflow) GetPrompt(ctx context.Context, ref agents.AgentRef, deps *agents.Dependencies) (string, error) {
	options, err := f.options(ctx, ref)
	if err != nil {
		return "", err
	}
	if options.Instruction == nil {
		return "", configError("agent %s has no instruction", ref)
	}
	return NewTemporalPrompt(options.Instruction, agents.PromptMiddlewaresOf(options.Middlewares)...).GetPrompt(ctx, deps)
}

func (f *FactoryAgentWorkflow) NewStreamingResponses(ctx context.Context, ref agents.AgentRef, in *responses.Request, call *agents.ModelCall) (*responses.Response, error) {
	options, err := f.options(ctx, ref)
	if err != nil {
		return nil, err
	}
	return NewTemporalLLM(options.LLM, f.broker, agents.ModelCallMiddlewaresOf(options.Middlewares)...).NewStreamingResponsesActivity(ctx, in, call)
}

func (f *FactoryAgentWorkflow) history(ctx context.Context, ref agents.AgentRef) (*agents.AgentOptions, *TemporalHistory, error) {
	options, err := f.options(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	if options.History == nil {
		return nil, nil, configError("agent %s has no History", ref)
	}
	return options, NewTemporalConversationPersistence(options.History.ConversationPersistenceAdapter, agents.HistoryMiddlewaresOf(options.Middlewares)...), nil
}

func (f *FactoryAgentWorkflow) LoadMessages(ctx context.Context, ref agents.AgentRef, namespace, threadID, previousRunID string) ([]history.ConversationMessage, error) {
	_, h, err := f.history(ctx, ref)
	if err != nil {
		return nil, err
	}
	return h.LoadMessages(ctx, namespace, threadID, previousRunID)
}

func (f *FactoryAgentWorkflow) SaveMessages(ctx context.Context, ref agents.AgentRef, namespace, groupID, runID, previousRunID, threadID, conversationID string, msgs []history.Message, meta map[string]any) error {
	_, h, err := f.history(ctx, ref)
	if err != nil {
		return err
	}
	return h.SaveMessages(ctx, namespace, groupID, runID, previousRunID, threadID, conversationID, msgs, meta)
}

func (f *FactoryAgentWorkflow) SaveSummary(ctx context.Context, ref agents.AgentRef, namespace string, summary history.Summary) error {
	_, h, err := f.history(ctx, ref)
	if err != nil {
		return err
	}
	return h.SaveSummary(ctx, namespace, summary)
}

func (f *FactoryAgentWorkflow) summarizer(ctx context.Context, ref agents.AgentRef) (history.HistorySummarizer, error) {
	options, _, err := f.history(ctx, ref)
	if err != nil {
		return nil, err
	}
	if options.History.Summarizer == nil {
		return nil, configError("agent %s has no summarizer", ref)
	}
	return options.History.Summarizer, nil
}

func (f *FactoryAgentWorkflow) Summarize(ctx context.Context, ref agents.AgentRef, ids map[string]string, msgs []messages.Message, tokens int) (*history.SummaryResult, error) {
	summarizer, err := f.summarizer(ctx, ref)
	if err != nil {
		return nil, err
	}
	return summarizer.Summarize(ctx, ids, msgs, tokens)
}

// ShouldSummarize is the local activity behind the workflow's summarization decision.
func (f *FactoryAgentWorkflow) ShouldSummarize(ctx context.Context, ref agents.AgentRef, ids map[string]string, msgs []messages.Message, tokens int) (bool, error) {
	summarizer, err := f.summarizer(ctx, ref)
	if err != nil {
		return false, err
	}
	return summarizer.ShouldSummarize(ctx, ids, msgs, tokens)
}

func (f *FactoryAgentWorkflow) FilterMessages(ctx context.Context, ref agents.AgentRef, msgs []messages.Message, agentID string) ([]messages.Message, error) {
	options, _, err := f.history(ctx, ref)
	if err != nil {
		return nil, err
	}
	if options.History.MessageFilter == nil {
		return msgs, nil
	}
	return options.History.MessageFilter.Filter(ctx, msgs, agentID), nil
}

func (f *FactoryAgentWorkflow) IsStopped(ctx context.Context, _ agents.AgentRef, channel string) (bool, error) {
	return f.broker.IsStopped(ctx, channel)
}

func (f *FactoryAgentWorkflow) DrainMessages(ctx context.Context, _ agents.AgentRef, channel string) ([]messages.Message, error) {
	return f.broker.DrainMessages(ctx, channel)
}

func (f *FactoryAgentWorkflow) WaitToolResult(ctx context.Context, _ agents.AgentRef, channel, callID string, timeout time.Duration) (toolResultWait, error) {
	return NewTemporalStreamBroker(f.broker).WaitToolResult(ctx, channel, callID, timeout)
}

func (f *FactoryAgentWorkflow) StreamHeartbeat(ctx context.Context, _ agents.AgentRef, channel string) error {
	return (&streamHeartbeatActivity{broker: f.broker}).Run(ctx, channel)
}

func (f *FactoryAgentWorkflow) tool(ctx context.Context, ref agents.AgentRef, key string) (*agents.AgentOptions, agents.Tool, error) {
	options, err := f.options(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	tool := agents.FindToolByKey(options.Tools, key)
	if tool == nil {
		return nil, nil, configError("agent %s has no tool %q", ref, key)
	}
	return options, tool, nil
}

func (f *FactoryAgentWorkflow) ExecuteTool(ctx context.Context, ref agents.AgentRef, key string, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	options, tool, err := f.tool(ctx, ref, key)
	if err != nil {
		return nil, err
	}
	return NewTemporalTool(tool, f.broker, agents.ToolCallMiddlewaresOf(options.Middlewares)...).Execute(ctx, call)
}

func (f *FactoryAgentWorkflow) AwaitTask(ctx context.Context, ref agents.AgentRef, key string, task agents.BackgroundTaskRef) (agents.BackgroundResult, error) {
	options, tool, err := f.tool(ctx, ref, key)
	if err != nil {
		return agents.BackgroundResult{}, err
	}
	background, ok := tool.(agents.BackgroundTool)
	if !ok {
		return agents.BackgroundResult{}, configError("tool %q of agent %s cannot wait for tasks", key, ref)
	}
	wrapped := agents.WrapBackgroundTool(ref.Name, background, agents.ToolCallMiddlewaresOf(options.Middlewares)...)
	return NewTemporalBackgroundTask(wrapped, f.broker).AwaitTask(ctx, task)
}

func (f *FactoryAgentWorkflow) CloseTaskStream(ctx context.Context, _ agents.AgentRef, task agents.BackgroundTaskRef) error {
	return NewTemporalBackgroundDelivery(f.broker).CloseTaskStream(ctx, task)
}

func (f *FactoryAgentWorkflow) DeliverTask(ctx context.Context, _ agents.AgentRef, in *DeliverTaskInput) (*DeliverTaskOutput, error) {
	return NewTemporalBackgroundDelivery(f.broker).DeliverTask(ctx, in)
}

func (f *FactoryAgentWorkflow) mcp(ctx context.Context, ref agents.AgentRef) (*TemporalMCPClient, error) {
	options, err := f.options(ctx, ref)
	if err != nil {
		return nil, err
	}
	if options.MCPClient == nil {
		return nil, configError("agent %s has no MCP client", ref)
	}
	return NewTemporalMCPClient(options.MCPClient, f.broker, agents.ToolCallMiddlewaresOf(options.Middlewares)...), nil
}

func (f *FactoryAgentWorkflow) ListMCPTools(ctx context.Context, ref agents.AgentRef, in *agents.AgentInput) (*agents.MCPListing, error) {
	client, err := f.mcp(ctx, ref)
	if err != nil {
		return nil, err
	}
	return client.ListTools(ctx, in)
}

func (f *FactoryAgentWorkflow) ExecuteMCPTool(ctx context.Context, ref agents.AgentRef, tool *agents.BaseTool, call *agents.ToolCall, runContext map[string]any) (*agents.ToolCallResponse, error) {
	client, err := f.mcp(ctx, ref)
	if err != nil {
		return nil, err
	}
	return client.ExecuteTool(ctx, tool, call, runContext)
}

func (f *FactoryAgentWorkflow) skills(ctx context.Context, ref agents.AgentRef) (*TemporalSkillClient, error) {
	options, err := f.options(ctx, ref)
	if err != nil {
		return nil, err
	}
	if options.SkillClient == nil {
		return nil, configError("agent %s has no skill client", ref)
	}
	return NewTemporalSkillClient(options.SkillClient, f.broker, agents.ToolCallMiddlewaresOf(options.Middlewares)...), nil
}

func (f *FactoryAgentWorkflow) ListSkills(ctx context.Context, ref agents.AgentRef, namespace string, rc map[string]any) ([]agents.Skill, error) {
	client, err := f.skills(ctx, ref)
	if err != nil {
		return nil, err
	}
	return client.ListSkills(ctx, namespace, rc)
}

func (f *FactoryAgentWorkflow) ReadSkill(ctx context.Context, ref agents.AgentRef, namespace string, rc map[string]any, name, file string, call *agents.ToolCall) (string, error) {
	client, err := f.skills(ctx, ref)
	if err != nil {
		return "", err
	}
	return client.ReadSkill(ctx, namespace, rc, name, file, call)
}
