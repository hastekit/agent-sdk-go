package restate_runtime

import (
	"context"
	"errors"
	"fmt"

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

	// ToolAgentName is the agent the tool is configured on, which after a
	// handoff is a specialist rather than the owner. The two are looked up for
	// different reasons and must not be conflated: restarting the specialist's
	// workflow would strand the result on a conversation of its own, and
	// looking for the tool on the owner would not find it.
	ToolAgentName string `json:"tool_agent_name,omitempty"`

	ToolName string                   `json:"tool_name"`
	Ref      agents.BackgroundTaskRef `json:"ref"`

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
	toolName := backgroundToolName(tool)
	if toolName == "" {
		return fmt.Errorf("background tool for task %s has no name, so the service cannot find it again", ref.TaskID)
	}

	// The runner belongs to the run's owner, so r.agentName names the workflow
	// to restart. Which agent to find the tool on comes from the ref, since
	// after a handoff that is a different agent entirely.
	toolAgent := ref.ToolAgentName
	if toolAgent == "" {
		toolAgent = r.agentName
	}

	restate.ServiceSend(r.restateCtx, BackgroundTaskServiceName, "Await").Send(&BackgroundTaskInput{
		AgentName:         r.agentName,
		ToolAgentName:     toolAgent,
		ToolName:          toolName,
		Ref:               ref,
		ProviderConfigKey: r.providerConfigKey,
	})
	return nil
}

// BackgroundTaskService waits for background tasks and delivers their results.
//
// It holds the same agent configs the workflow does, because the tool that
// knows how to wait for a task is the agent's own — and the tool itself, not a
// proxy of it, since this is where the waiting actually happens.
type BackgroundTaskService struct {
	agentConfigs map[string]*agents.AgentOptions
	broker       agents.StreamBroker
}

func NewBackgroundTaskService(agentConfigs map[string]*agents.AgentOptions, broker agents.StreamBroker) *BackgroundTaskService {
	return &BackgroundTaskService{agentConfigs: agentConfigs, broker: broker}
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
	}, restate.WithName(in.ToolName+"_AwaitTask"))
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

// backgroundTool finds the tool that knows how to wait for this task.
//
// It looks on ToolAgentName, not AgentName: after a handoff the tool belongs
// to the specialist while the run belongs to the agent it entered at.
func (s *BackgroundTaskService) backgroundTool(in *BackgroundTaskInput) (agents.BackgroundTool, error) {
	agentName := in.ToolAgentName
	if agentName == "" {
		agentName = in.AgentName
	}

	options, ok := s.agentConfigs[agentName]
	if !ok {
		return nil, fmt.Errorf("agent not found: %s", agentName)
	}

	for _, tool := range agents.WithSkillTool(options.Tools, options.Skills) {
		if backgroundToolName(tool) != in.ToolName {
			continue
		}
		if backgroundTool, ok := tool.(agents.BackgroundTool); ok {
			return backgroundTool, nil
		}
		return nil, fmt.Errorf("tool %q on agent %q does not wait for background tasks", in.ToolName, agentName)
	}

	return nil, fmt.Errorf("tool %q not found on agent %q", in.ToolName, agentName)
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
