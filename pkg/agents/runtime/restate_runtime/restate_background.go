package restate_runtime

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	restate "github.com/restatedev/sdk-go"
)

// BackgroundTaskServiceName is the service that waits for background tasks.
// It is a plain service rather than a workflow: each task is one invocation,
// and Restate keeps an invocation alive across restarts on its own.
const BackgroundTaskServiceName = "BackgroundTaskService"

// BackgroundTaskInput is what the background service is invoked with: which
// agent's tool to ask, and about what.
type BackgroundTaskInput struct {
	// AgentName is the agent whose workflow a run started by this task's
	// result enters at — the one that owns the thread.
	AgentName string `json:"agent_name"`

	// ToolKey identifies the registered wait operation, including its middleware.
	// It is supplied by the tool proxy, independently of the delivery agent.
	ToolKey string                   `json:"tool_key"`
	Ref     agents.BackgroundTaskRef `json:"ref"`

	// ProviderConfigKey travels with the task for the same reason it travels
	// with a run: Restate has no context propagator, so a run started by this
	// task's result would otherwise reach the gateway without a credential.
	ProviderConfigKey string `json:"provider_config_key,omitempty"`
}

// RestateBackgroundRunner hands a task's wait to a service invocation of its
// own, so it outlives the run that started it.
//
// The send is one-way: the run does not wait for the task, and Restate keeps
// the invocation going whether or not this workflow is still around.
type RestateBackgroundRunner struct {
	restateCtx        restate.WorkflowContext
	agentName         string
	providerConfigKey string
}

var _ agents.BackgroundRunner = (*RestateBackgroundRunner)(nil)

func NewRestateBackgroundRunner(restateCtx restate.WorkflowContext, agentName, providerConfigKey string) *RestateBackgroundRunner {
	return &RestateBackgroundRunner{
		restateCtx:        restateCtx,
		agentName:         agentName,
		providerConfigKey: providerConfigKey,
	}
}

func (r *RestateBackgroundRunner) StartTask(_ context.Context, tool agents.BackgroundTool, ref agents.BackgroundTaskRef) error {
	proxy, ok := tool.(*RestateBackgroundTool)
	if !ok || proxy.key == "" {
		return fmt.Errorf("background tool for task %s is not a registered workflow proxy", ref.TaskID)
	}

	restate.ServiceSend(r.restateCtx, BackgroundTaskServiceName, "Await").Send(&BackgroundTaskInput{
		AgentName:         r.agentName,
		ToolKey:           proxy.key,
		Ref:               ref,
		ProviderConfigKey: r.providerConfigKey,
	})
	return nil
}

// BackgroundTaskService waits for background tasks and delivers their results.
//
// Tools and middleware are bound once at registration. Invocations only carry
// the key of that operation; task delivery still uses the owner's agent name.
type BackgroundTaskService struct {
	tools  map[string]agents.BackgroundTool
	broker agents.StreamBroker
}

func NewBackgroundTaskService(agentConfigs map[string]*agents.AgentOptions, broker agents.StreamBroker) *BackgroundTaskService {
	s := &BackgroundTaskService{tools: make(map[string]agents.BackgroundTool), broker: broker}
	for name, options := range agentConfigs {
		for _, tool := range agents.WithSkillTool(options.Tools, options.Skills) {
			if background, ok := tool.(agents.BackgroundTool); ok {
				s.tools[backgroundToolKey(name, backgroundToolName(tool))] = agents.WrapBackgroundTool(name, background, agents.ToolCallMiddlewaresOf(options.Middlewares)...)
			}
		}
	}
	return s
}

// Await waits for one task, then puts its outcome in front of the agent.
func (s *BackgroundTaskService) Await(ctx restate.Context, in *BackgroundTaskInput) (restate.Void, error) {
	tool, err := s.backgroundTool(in)
	if err != nil {
		return restate.Void{}, err
	}

	// One durable step for the wait. It is a long one by design — that is what
	// makes the task a background task — and its outcome is journaled, so a
	// restart after it finishes does not wait for it again.
	outcome, err := restate.Run(ctx, func(runCtx restate.RunContext) (*awaitOutcome, error) {
		progress := agents.NewStreamProgressReporter(s.broker, in.Ref.TaskStreamID, in.Ref.CallID, in.Ref.ToolName)
		res, err := tool.AwaitTask(runCtx, in.Ref, progress)
		if err != nil {
			// The wait's failure is what the model is told about, not a step
			// to retry — so it travels as its own field rather than as an
			// error, and rather than masquerading as the task's result.
			return &awaitOutcome{Fail: err.Error()}, nil
		}
		return &awaitOutcome{Result: res}, nil
	}, restate.WithName(in.ToolKey+"_AwaitTask"))
	if err != nil {
		return restate.Void{}, err
	}

	var awaitErr error
	if outcome.Fail != "" {
		awaitErr = errors.New(outcome.Fail)
	}

	// Close first: a client watching the task sees it end when it ends, not
	// when the agent has finished reacting to it.
	if _, err := restate.Run(ctx, func(runCtx restate.RunContext) (restate.Void, error) {
		return restate.Void{}, agents.CloseBackgroundTaskStream(runCtx, s.broker, in.Ref)
	}, restate.WithName("CloseTaskStream")); err != nil {
		return restate.Void{}, err
	}

	decision, err := restate.Run(ctx, func(runCtx restate.RunContext) (*DeliverTaskOutput, error) {
		start, msg, err := agents.DeliverBackgroundResult(runCtx, s.broker, in.Ref, outcome.Result, awaitErr)
		if err != nil {
			return nil, err
		}
		return &DeliverTaskOutput{Start: start, Message: msg}, nil
	}, restate.WithName("DeliverTask"))
	if err != nil {
		return restate.Void{}, err
	}

	if !decision.Start {
		// Folded into a run that is already going, or onto the queue of one
		// about to start. Either way somebody else reads it.
		return restate.Void{}, nil
	}

	// The thread was idle, and this invocation now holds its claim. Start the
	// run one-way: this task is done, and waiting for the model to finish
	// talking is not part of it.
	restate.WorkflowSend(ctx, "AgentWorkflow", in.Ref.ThreadStreamID, "Run").Send(&WorkflowInput{
		AgentName:         in.AgentName,
		Namespace:         in.Ref.Namespace,
		ThreadID:          in.Ref.ThreadID,
		Message:           decision.Message,
		RunContext:        in.Ref.RunContext,
		StreamID:          in.Ref.ThreadStreamID,
		ProviderConfigKey: in.ProviderConfigKey,
	})

	return restate.Void{}, nil
}

// awaitOutcome is what the wait step journals: the task's result, or the
// reason the wait itself broke down. Both are news for the model; neither is a
// step for Restate to retry.
type awaitOutcome struct {
	Result agents.BackgroundResult `json:"result"`
	Fail   string                  `json:"fail,omitempty"`
}

// DeliverTaskOutput says whether the caller must now start a run.
type DeliverTaskOutput struct {
	Start   bool            `json:"start"`
	Message history.Message `json:"message"`
}

// backgroundTool resolves an execution binding, not an agent's runtime policy.
func (s *BackgroundTaskService) backgroundTool(in *BackgroundTaskInput) (agents.BackgroundTool, error) {
	tool, ok := s.tools[in.ToolKey]
	if !ok {
		return nil, fmt.Errorf("background tool %q is not registered", in.ToolKey)
	}
	return tool, nil
}

func backgroundToolKey(agentName, toolName string) string {
	return url.PathEscape(agentName) + "/" + url.PathEscape(toolName)
}

// backgroundToolName reads a tool's own name, which is how the service finds
// it again on the far side of the send.
func backgroundToolName(tool agents.Tool) string {
	descriptor := tool.GetToolDescriptor()
	if descriptor == nil || descriptor.ToolUnion.OfFunction == nil {
		return ""
	}
	return descriptor.ToolUnion.OfFunction.Name
}
