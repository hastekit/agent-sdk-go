package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type SubAgentContextMode string

const (
	SubAgentContextModeNone     SubAgentContextMode = "None"
	SubAgentContextModeIsolated SubAgentContextMode = "Isolated"
)

// AgentTool lets a model hand a message to another agent, which works on it in
// a thread of its own: hidden, grouped under and parented to the calling
// thread, and sharing its session. In SubAgentContextModeNone the model passes
// a thread_id back to follow up in the same thread; in
// SubAgentContextModeIsolated each calling thread keeps one sub-agent thread.
//
// Whether the caller waits is fixed by the constructor, not chosen by the
// model. With NewAgentTool it waits: the sub-agent's answer is the tool's
// result, and a sub-agent that stops for approval stops the caller with it.
// With NewAsyncAgentTool it does not: the tool answers at once with the
// sub-agent's thread id, the sub-agent works in parallel, and its final answer
// is delivered to the calling thread when it is done — into the run still
// going, or as a new run. An async sub-agent cannot stop for approval or
// input, as nobody is waiting on it to answer; one that pauses reports so as
// its answer.
//
// Either way, a follow-up to a sub-agent still working on an async task is
// steered into that task: it joins the run at its next step, and the task's
// answer covers it.
type AgentTool struct {
	*agents.BaseTool
	agent          *agents.Agent
	contextMode    SubAgentContextMode
	withoutTracing bool
	async          bool
}

// AsyncAgentTool is an AgentTool whose calls do not wait for the sub-agent.
// It is the one that can be waited on in the background, which is what a
// runtime looks for to keep the wait alive past the call.
type AsyncAgentTool struct {
	*AgentTool
}

var _ agents.BackgroundTool = (*AsyncAgentTool)(nil)

type agentToolArgument struct {
	Message  string `json:"message"`
	ThreadID string `json:"thread_id"`
}

type AgentToolOption func(*AgentTool)

func WithoutTracing(t *AgentTool) {
	t.withoutTracing = true
}

// withAsync makes calls not wait for the sub-agent's answer.
func withAsync() AgentToolOption {
	return func(t *AgentTool) { t.async = true }
}

// NewAgentTool hands the model's message to agent and waits for its answer.
func NewAgentTool(name string, description string, agent *agents.Agent, contextMode SubAgentContextMode, opts ...AgentToolOption) *AgentTool {
	return newAgentTool(name, description, agent, contextMode, opts...)
}

// NewAsyncAgentTool hands the model's message to agent and returns at once;
// the agent's answer is delivered to the calling thread when it is done.
func NewAsyncAgentTool(name string, description string, agent *agents.Agent, contextMode SubAgentContextMode, opts ...AgentToolOption) *AsyncAgentTool {
	return &AsyncAgentTool{AgentTool: newAgentTool(name, description, agent, contextMode, append(opts, withAsync())...)}
}

func newAgentTool(name string, description string, agent *agents.Agent, contextMode SubAgentContextMode, opts ...AgentToolOption) *AgentTool {
	at := &AgentTool{agent: agent, contextMode: contextMode}
	for _, opt := range opts {
		opt(at)
	}

	properties := map[string]any{
		"message": map[string]any{
			"type":        "string",
			"description": "Message for the agent",
		},
	}
	if contextMode == SubAgentContextModeNone {
		properties["thread_id"] = map[string]any{
			"type":        "string",
			"description": "Thread ID for the agent conversation. Leave empty to start a new conversation.",
		}
	}
	at.BaseTool = &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
		Name:        name,
		Description: utils.Ptr(description),
		Parameters: map[string]any{
			"type":                 "object",
			"required":             []string{"message"},
			"properties":           properties,
			"additionalProperties": false,
		},
	}}}
	return at
}

// Execute runs the inner agent. When params.ShouldResume is set,
// continues a previously paused call instead of starting a fresh
// one — recovers the inner thread id and prior agents.AgentOutput from
// params.State (written on the earlier pause), then re-enters the
// inner agent with params.ResumeMessages (typically a single
// FunctionCallInterruptResolutionMessage). Otherwise, parses the LLM's
// arguments, picks/derives a thread id per contextMode, and either starts
// the inner agent in the background (async) or runs it to its answer.
//
// A waited-on result is shaped by responseFromResult: a paused inner
// re-emits its Interrupts (and refreshes the saved state entries on
// params.State for the next resume); a completed inner produces the outer
// call's FunctionCallOutputMessage so the outer history regains its
// function_call ↔ function_call_output pair.
func (t *AgentTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if params.ShouldResume {
		return t.resume(ctx, params)
	}

	var args agentToolArgument
	if err := sonic.Unmarshal([]byte(params.Arguments), &args); err != nil {
		return nil, err
	}

	// An async call's id is fixed by the call, so a retried call starts the
	// same task; a new thread takes it as its id too.
	newThreadID := uuid.NewString()
	if t.async {
		newThreadID = callDerivedID(params)
	}

	threadID, existing := t.threadFor(params, args, newThreadID)
	if existing {
		steered, err := t.steer(ctx, params, threadID, args.Message)
		if err != nil || steered != nil {
			return steered, err
		}
	}

	if t.async {
		return t.start(params, args.Message, threadID)
	}

	return t.run(ctx, params, t.input(params, threadID, "", history.Message{
		SenderID: params.AgentName,
		Messages: []responses.InputMessageUnion{userText(args.Message)},
	}))
}

// threadFor is the thread a call goes to, and whether it is one already
// started.
func (t *AgentTool) threadFor(params *agents.ToolCall, args agentToolArgument, newThreadID string) (string, bool) {
	switch {
	case t.contextMode == SubAgentContextModeIsolated && params.State[t.getSubAgentThreadIdStateKey()] != "":
		return params.State[t.getSubAgentThreadIdStateKey()], true
	case t.contextMode == SubAgentContextModeNone && args.ThreadID != "":
		return args.ThreadID, true
	}
	return newThreadID, false
}

// resume continues a waited-on call whose sub-agent paused.
func (t *AgentTool) resume(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if params.State == nil {
		return nil, fmt.Errorf("agent_tool: cannot resume — params.State missing")
	}

	runStateRaw, ok := params.State[t.getRunStateKey(params.ID)]
	if !ok || runStateRaw == "" {
		return nil, fmt.Errorf("agent_tool: cannot resume — saved run state missing for tool call %s", params.ID)
	}
	var savedResult agents.AgentOutput
	if err := sonic.Unmarshal([]byte(runStateRaw), &savedResult); err != nil {
		return nil, fmt.Errorf("agent_tool: malformed saved run state: %w", err)
	}
	if savedResult.RunID == "" {
		return nil, fmt.Errorf("agent_tool: saved run state has empty RunID for tool call %s", params.ID)
	}

	savedThreadId, ok := params.State[t.getResumeThreadIdStateKey(params.ID)]
	if !ok || savedThreadId == "" {
		return nil, fmt.Errorf("agent_tool: cannot resume — thread id missing for tool call %s", params.ID)
	}

	return t.run(ctx, params, t.input(params, savedThreadId, savedResult.RunID, history.Message{
		SenderID: params.AgentName,
		Messages: params.ResumeMessages,
	}))
}

// input is the sub-agent's run: in the caller's namespace, in the group the
// call names for new threads (the calling thread's), parented to the calling
// thread, hidden from listing UIs, and sharing the caller's session for its
// attachments (not its history).
func (t *AgentTool) input(params *agents.ToolCall, threadID, previousRunID string, message history.Message) *agents.AgentInput {
	return &agents.AgentInput{
		Namespace:      params.Namespace,
		GroupID:        params.NewThreadGroupID(),
		ParentThreadID: params.ThreadID,
		Hidden:         true,
		ThreadID:       threadID,
		PreviousRunID:  previousRunID,
		Message:        message,
		SessionID:      params.SessionID,
	}
}

// run runs the sub-agent to its answer, which is the call's result.
func (t *AgentTool) run(ctx context.Context, params *agents.ToolCall, input *agents.AgentInput) (*agents.ToolCallResponse, error) {
	var result *agents.AgentOutput
	var err error
	if t.withoutTracing {
		result, err = t.agent.ExecuteWithoutTrace(ctx, input)
		if err != nil {
			return nil, err
		}
	} else {
		handle, err := t.agent.Execute(ctx, input)
		if err != nil {
			return nil, err
		}

		// The parent agent reports tool output, not chunks. Result drains
		// the sub-agent's chunk stream and returns the aggregated output.
		result, err = handle.Result()
		if err != nil {
			return nil, err
		}
	}

	return t.responseFromResult(params, result, input.ThreadID)
}

// agentTaskPayload travels from an async call to AwaitTask, which under a
// durable runtime runs in another process.
type agentTaskPayload struct {
	ThreadID  string `json:"thread_id"`
	GroupID   string `json:"group_id"`
	MessageID string `json:"message_id"`
	SenderID  string `json:"sender_id"`
	Message   string `json:"message"`
}

// start answers an async call at once and leaves the task for AwaitTask.
func (t *AgentTool) start(params *agents.ToolCall, message, threadID string) (*agents.ToolCallResponse, error) {
	taskID := callDerivedID(params)
	payload, err := json.Marshal(agentTaskPayload{ThreadID: threadID, GroupID: params.NewThreadGroupID(), MessageID: taskID, SenderID: params.AgentName, Message: message})
	if err != nil {
		return nil, err
	}
	text := t.withThreadID(fmt.Sprintf("Started %s. It is working in the background; its answer will arrive on its own when it is done, so carry on rather than waiting.", t.agent.Name), threadID)
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(text)},
		},
		StateUpdates: map[string]string{t.getSubAgentThreadIdStateKey(): threadID},
		TaskID:       taskID,
		TaskPayload:  payload,
	}, nil
}

// AwaitTask runs an async call's sub-agent thread to its end and reports its
// answer.
func (t *AsyncAgentTool) AwaitTask(ctx context.Context, task agents.BackgroundTaskRef, _ agents.ProgressReporter) (agents.BackgroundResult, error) {
	var payload agentTaskPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return agents.BackgroundResult{}, fmt.Errorf("agent task %s: reading its payload: %w", task.TaskID, err)
	}

	input := t.input(&agents.ToolCall{Namespace: task.Namespace, ThreadID: task.ThreadID, GroupID: payload.GroupID, SessionID: task.SessionID}, payload.ThreadID, "", history.Message{
		ID:       payload.MessageID,
		SenderID: payload.SenderID,
		Messages: []responses.InputMessageUnion{userText(payload.Message)},
	})
	// The thread's own channel, so a client can rejoin it and a second
	// message cannot start a second run on it.
	input.StreamID = agents.StreamIDForThread(task.Namespace, payload.ThreadID)

	// Safe to retry: a wait retried after its worker died finds the turn the
	// first attempt started instead of starting it again. A follow-up that
	// raced past Execute's check joins the run already going.
	outcome, err := agents.RunAgentTask(ctx, t.agent, input)
	if err != nil {
		return agents.BackgroundResult{}, fmt.Errorf("agent task %s: %w", task.TaskID, err)
	}
	if outcome.Joined {
		return agents.BackgroundResult{Output: agents.BackgroundText(t.withThreadID(
			"The agent was still working, so this message was added to its current task; its answer to that task will cover it.", payload.ThreadID))}, nil
	}
	result := outcome.Output
	if result != nil && result.Status == agentstate.RunStatusPaused {
		return agents.BackgroundResult{Output: agents.BackgroundText(t.withThreadID(
			"The agent stopped to ask for approval or input, which cannot be given to an agent working in the background, so its task is unfinished.", payload.ThreadID))}, nil
	}
	return agents.BackgroundResult{Output: agents.BackgroundText(t.withThreadID(agentOutputText(result), payload.ThreadID))}, nil
}

// steer folds a follow-up into the run already going on the sub-agent's
// thread, as a user's message joins a busy conversation: the run takes it at
// its next step, and the answer its task delivers covers it. It answers nil
// when no run is going there.
//
// A run that ends between the check and the enqueue leaves the message on the
// thread's queue for its next turn rather than starting one on a guess.
func (t *AgentTool) steer(ctx context.Context, params *agents.ToolCall, threadID, message string) (*agents.ToolCallResponse, error) {
	// Get the stream broker
	broker := t.agent.StreamBroker()
	if broker == nil {
		return nil, nil
	}

	// Get the idempotent stream id for the thread
	stream := agents.StreamIDForThread(params.Namespace, threadID)

	// Check if the stream is active
	active, err := broker.IsActive(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("check the agent on thread %s: %w", threadID, err)
	}

	// If not return early
	if !active {
		return nil, nil
	}

	// If it is active, try to enqueue the message for steering
	if err := broker.EnqueueMessage(ctx, stream, history.Message{
		ID:       callDerivedID(params),
		SenderID: params.AgentName,
		Messages: []responses.InputMessageUnion{userText(message)},
	}); err != nil {
		return nil, fmt.Errorf("send to the agent on thread %s: %w", threadID, err)
	}

	text := t.withThreadID(fmt.Sprintf("%s is still working on its current task, so this message was added to it; the answer to that task will arrive on its own and cover it.", t.agent.Name), threadID)

	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(text)},
		},
		StateUpdates: map[string]string{t.getSubAgentThreadIdStateKey(): threadID},
	}, nil
}

// responseFromResult shapes an inner agents.AgentOutput into the outer
// agents.ToolCallResponse. Shared between Execute and Resume so the two
// paths stay in lockstep on shape, state-key naming, and the
// pause-vs-completion branch.
func (t *AgentTool) responseFromResult(params *agents.ToolCall, result *agents.AgentOutput, threadId string) (*agents.ToolCallResponse, error) {
	if result != nil && result.Status == agentstate.RunStatusPaused {
		resultBuf, err := sonic.Marshal(result)
		if err != nil {
			return nil, err
		}
		return &agents.ToolCallResponse{
			StateUpdates: map[string]string{
				t.getResumeThreadIdStateKey(params.ID): threadId,
				t.getRunStateKey(params.ID):            string(resultBuf),
			},
			Interrupts: result.Interrupts,
		}, nil
	}

	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(t.withThreadID(agentOutputText(result), threadId)),
			},
		},
		StateUpdates: map[string]string{
			t.getSubAgentThreadIdStateKey(): threadId,
		},
	}, nil
}

// withThreadID adds the thread id where the model can send a follow-up to it.
func (t *AgentTool) withThreadID(text, threadID string) string {
	if t.contextMode != SubAgentContextModeNone {
		return text
	}
	return text + fmt.Sprintf("\n---\nThread ID: %s", threadID)
}

func (t *AgentTool) getSubAgentThreadIdStateKey() string {
	return fmt.Sprintf("sub_agent_thread_id/%s", t.agent.Name)
}

func (t *AgentTool) getResumeThreadIdStateKey(toolCallId string) string {
	return fmt.Sprintf("resume_thread_id/%s/%s", t.agent.Name, toolCallId)
}

func (t *AgentTool) getRunStateKey(toolCallId string) string {
	return fmt.Sprintf("run_state/%s/%s", t.agent.Name, toolCallId)
}

func userText(text string) responses.InputMessageUnion {
	return responses.InputMessageUnion{OfEasyInput: &responses.EasyMessage{
		Role:    constants.RoleUser,
		Content: responses.EasyInputContentUnion{OfString: &text},
	}}
}

// agentOutputText is the sub-agent's answer: the text of its output.
func agentOutputText(result *agents.AgentOutput) string {
	var data strings.Builder
	if result == nil {
		return ""
	}
	for _, out := range result.Output {
		if out.OfOutputMessage != nil {
			for _, content := range *out.OfOutputMessage.Content {
				if content.OfOutputText != nil {
					data.WriteString(content.OfOutputText.Text)
				}
			}
		}

		if out.OfEasyInput != nil {
			if out.OfEasyInput.Content.OfString != nil {
				data.WriteString(*out.OfEasyInput.Content.OfString)
			}
			for _, message := range out.OfEasyInput.Content.OfInputMessageList {
				if message.OfOutputText != nil {
					data.WriteString(message.OfOutputText.Text)
				}
			}
		}
	}
	return data.String()
}

// callDerivedID is an id fixed by the calling thread and tool call, so a
// retried call reuses it.
func callDerivedID(call *agents.ToolCall) string {
	callID := call.CallID
	if callID == "" {
		callID = call.ID
	}
	if callID == "" {
		return uuid.NewString()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(call.Namespace+"\x00"+call.ThreadID+"\x00"+callID)).String()
}
