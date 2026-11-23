package temporal_runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentfactory"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	// FactoryAgentWorkflowName is the one workflow every factory agent runs on, started with (spec, input).
	FactoryAgentWorkflowName = "hastekit_FactoryAgentWorkflow"
	// FactoryBackgroundTaskWorkflowName waits on a background task a factory agent started.
	FactoryBackgroundTaskWorkflowName = factoryPrefix + backgroundWorkflowSuffix
	// AgentNotFoundErrorType marks a handoff target the loader does not know; it is not retried.
	AgentNotFoundErrorType = "AgentNotFound"
	// AgentSpecErrorType marks a spec this worker cannot run, such as an unknown tool; it is not retried.
	AgentSpecErrorType = "AgentSpec"

	factoryPrefix         = "hastekit_Factory"
	loadAgentActivity     = factoryPrefix + "_LoadAgentActivity"
	factoryPromptActivity = factoryPrefix + "_GetPromptActivity"
	factoryModelActivity  = factoryPrefix + "_NewStreamingResponsesActivity"
)

// FactoryAgentWorkflow runs factory agents from the spec in their workflow input, so agents
// stored after the worker started need no registration, and replay never reloads them.
type FactoryAgentWorkflow struct {
	factory *agents.AgentFactory
	broker  agents.StreamBroker
}

func NewFactoryAgentWorkflow(factory *agents.AgentFactory, broker agents.StreamBroker) (*FactoryAgentWorkflow, error) {
	if err := factory.Validate(); err != nil {
		return nil, err
	}
	if factory.History == nil {
		return nil, errors.New("agent factory has no History; durable runs need persistent history")
	}
	return &FactoryAgentWorkflow{factory: factory, broker: broker}, nil
}

// GetWorkflows returns the factory workflows to register, by name.
func (f *FactoryAgentWorkflow) GetWorkflows() map[string]any {
	return map[string]any{
		FactoryAgentWorkflowName:          f.Execute,
		FactoryBackgroundTaskWorkflowName: NewBackgroundTaskWorkflow(factoryPrefix, f.broker).Execute,
	}
}

// GetActivities returns the activities every factory agent shares. Only the prompt and model
// activities take spec data; the rest are the worker's own, as for a registered agent.
func (f *FactoryAgentWorkflow) GetActivities() map[string]any {
	activities := workerActivities(factoryPrefix, &agents.AgentOptions{
		History:     f.factory.History,
		Tools:       f.factory.Tools,
		Middlewares: f.factory.Middlewares,
	}, f.broker)
	activities[loadAgentActivity] = f.LoadAgent
	activities[factoryPromptActivity] = f.GetPrompt
	activities[factoryModelActivity] = f.NewStreamingResponses
	return activities
}

// Execute runs spec. The spec is the run's input, so a restart or replay runs what the run started with.
func (f *FactoryAgentWorkflow) Execute(ctx workflow.Context, spec *agents.AgentSpec, in *agents.AgentInput) (*agents.AgentOutput, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Second})
	if in.StreamID == "" {
		in.StreamID = workflow.GetInfo(ctx).WorkflowExecution.ID
	}
	agent, err := f.workflowAgent(ctx, spec)
	if err != nil {
		return &agents.AgentOutput{Status: agentstate.RunStatusError}, err
	}

	// Bridge the workflow span into the loop's context, as the registered workflow does.
	goCtx := context.Background()
	if span, ok := opentelemetry.SpanFromWorkflowContext(ctx); ok {
		goCtx = trace.ContextWithSpan(goCtx, span)
	}
	return agent.ExecuteWithoutTrace(goCtx, in)
}

// workflowAgent builds spec inside the workflow. Everything that leaves the process is an activity.
func (f *FactoryAgentWorkflow) workflowAgent(ctx workflow.Context, spec *agents.AgentSpec) (*agents.Agent, error) {
	p := factoryPrefix
	stored := f.factory.History

	var historyOptions []history.ConversationManagerOptions
	if stored.Summarizer != nil {
		historyOptions = append(historyOptions, history.WithSummarizer(NewTemporalConversationSummarizerProxy(ctx, p, stored.Summarizer)))
	}
	if stored.MessageFilter != nil {
		historyOptions = append(historyOptions, history.WithMessageFilter(NewTemporalMessageFilterProxy(ctx, p)))
	}
	conversations := history.NewConversationManager(NewTemporalConversationPersistenceProxy(ctx, p), historyOptions...)
	conversations.MessageAttribution = stored.MessageAttribution
	conversations.SteeringNotices = stored.SteeringNotices

	tools := make([]agents.Tool, 0, len(spec.Tools))
	for _, name := range spec.Tools {
		tool := f.factory.Tool(name)
		if tool == nil {
			return nil, specError(fmt.Errorf("agent %s: unknown tool %q", spec.Ref(), name))
		}
		tools = append(tools, NewTemporalToolProxy(ctx, getToolName(p, tool), tool))
	}

	handoffs := make([]*agents.Handoff, 0, len(spec.Handoffs))
	for _, h := range spec.Handoffs {
		target := &workflowHandoff{f: f, ctx: ctx, ref: h.Ref()}
		handoffs = append(handoffs, &agents.Handoff{Name: h.Name, Description: h.Description, Agent: target})
	}

	var instruction agents.SystemPromptProvider
	if spec.Instruction != "" {
		instruction = &factoryPromptProxy{ctx: ctx, instruction: spec.Instruction}
	}

	return agents.NewAgent(&agents.AgentOptions{
		Name:             spec.Name,
		Spec:             spec,
		Instruction:      instruction,
		Parameters:       spec.Parameters,
		Output:           spec.Output,
		MaxLoops:         agentfactory.MaxLoops(spec),
		History:          conversations,
		Tools:            tools,
		Handoffs:         handoffs,
		ToolExecutor:     NewTemporalToolExecutor(ctx),
		StreamBroker:     NewTemporalStreamBrokerProxy(ctx, p, f.broker),
		DurableStep:      NewTemporalDurableStep(ctx),
		BackgroundRunner: &TemporalBackgroundRunner{workflowCtx: ctx, agentName: p, spec: spec},
	}).WithLLM(&factoryModelProxy{ctx: ctx, model: spec.Model}), nil
}

// workflowHandoff loads its target when the model hands off. The load is journaled, so replay
// gets the same spec, and specs may hand off to each other in cycles.
type workflowHandoff struct {
	f   *FactoryAgentWorkflow
	ctx workflow.Context
	ref agents.AgentRef
}

func (h *workflowHandoff) ExecuteWithRun(ctx context.Context, in *agents.AgentInput, run *history.ConversationRunManager) (*agents.AgentOutput, error) {
	failed := &agents.AgentOutput{Status: agentstate.RunStatusError, RunID: run.GetRunID()}
	var spec agents.AgentSpec
	if err := workflow.ExecuteActivity(h.ctx, loadAgentActivity, h.ref).Get(h.ctx, &spec); err != nil {
		return failed, fmt.Errorf("handoff to %s: %w", h.ref, err)
	}
	target, err := h.f.workflowAgent(h.ctx, &spec)
	if err != nil {
		return failed, err
	}
	return target.ExecuteWithRun(ctx, in, run)
}

// factoryPromptProxy renders the spec's instruction in an activity, so prompt middlewares run there.
type factoryPromptProxy struct {
	ctx         workflow.Context
	instruction string
}

func (p *factoryPromptProxy) GetPrompt(_ context.Context, deps *agents.Dependencies) (string, error) {
	var prompt string
	err := workflow.ExecuteActivity(p.ctx, factoryPromptActivity, p.instruction, deps).Get(p.ctx, &prompt)
	return prompt, err
}

// factoryModelProxy calls the spec's model by id; the worker resolves it, so keys stay there.
type factoryModelProxy struct {
	ctx   workflow.Context
	model string
}

func (m *factoryModelProxy) NewStreamingResponses(_ context.Context, call *agents.ModelCall, in *responses.Request, _ func(*responses.ResponseChunk)) (*responses.Response, error) {
	var response *responses.Response
	err := workflow.ExecuteActivity(m.ctx, factoryModelActivity, m.model, in, call).Get(m.ctx, &response)
	if err != nil {
		// A stopped call comes back as an activity failure; report it as the stop it is.
		if wasCancelled(err) {
			return nil, agents.ErrModelCallStopped
		}
		return nil, err
	}
	return response, nil
}

// LoadAgent loads a handoff target's spec. An unknown or invalid spec fails without retries.
func (f *FactoryAgentWorkflow) LoadAgent(ctx context.Context, ref agents.AgentRef) (*agents.AgentSpec, error) {
	spec, err := f.factory.LoadSpec(ctx, ref)
	switch {
	case errors.Is(err, agents.ErrAgentNotFound):
		return nil, temporal.NewNonRetryableApplicationError(err.Error(), AgentNotFoundErrorType, err)
	case errors.Is(err, agents.ErrInvalidAgentSpec):
		return nil, specError(err)
	}
	return spec, err
}

func (f *FactoryAgentWorkflow) GetPrompt(ctx context.Context, instruction string, deps *agents.Dependencies) (string, error) {
	prompt := NewTemporalPrompt(agentfactory.Instruction(instruction), agents.PromptMiddlewaresOf(f.factory.Middlewares)...)
	return prompt.GetPrompt(ctx, deps)
}

func (f *FactoryAgentWorkflow) NewStreamingResponses(ctx context.Context, model string, in *responses.Request, call *agents.ModelCall) (*responses.Response, error) {
	provider, err := f.factory.Models.ResolveModel(model)
	if err != nil {
		return nil, specError(fmt.Errorf("model %q: %w", model, err))
	}
	return NewTemporalLLM(provider, f.broker, agents.ModelCallMiddlewaresOf(f.factory.Middlewares)...).NewStreamingResponsesActivity(ctx, in, call)
}

func specError(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), AgentSpecErrorType, err)
}
