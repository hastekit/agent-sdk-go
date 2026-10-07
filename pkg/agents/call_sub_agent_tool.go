package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// CallSubAgentToolName is the tool AgentOptions.SubAgents gives an agent.
const CallSubAgentToolName = "call_sub_agent"

// The modes a call_sub_agent call runs in.
const (
	SubAgentModeSync  = "sync"
	SubAgentModeAsync = "async"
)

const callSubAgentDescription = "Hand a message to one of your sub-agents, listed in your instructions. " +
	"It works on it in a thread of its own and does not see this conversation, so include everything it needs. " +
	"In sync mode the call waits and returns the sub-agent's answer. " +
	"In async mode it returns at once and the sub-agent works in parallel; its answer arrives on its own when it is done, so carry on rather than waiting. " +
	"Either way the result ends with the sub-agent's thread ID. " +
	"To follow up with the same sub-agent, call again with that thread_id: while it is still working the message joins its current task, otherwise it starts a new one there. " +
	"Leave thread_id empty to start a new thread."

// CallSubAgentTool lets an agent hand a message to one of the agents its
// SubAgentClient lists, waiting for the answer (sync) or not (async), as the
// model chooses per call. The client decides who may be called and how a
// message reaches them; the tool runs the call.
//
// The sub-agent works in a thread of its own: hidden, in the caller's
// namespace, grouped under the group the call names and parented to the
// calling thread, and sharing the caller's session (for its attachments, not
// its history). Passing its thread id back sends a follow-up into the same
// thread; one sent while the sub-agent is still working is steered into its
// current task, whose answer covers it.
//
// An async call answers at once with the thread id, and the sub-agent's final
// answer is delivered to the calling thread when it is done — into the run
// still going, or as a new run. A sub-agent cannot stop for approval or input
// in either mode; one that pauses reports so as its answer.
//
// NewAgent builds one, pointed at the agent itself, when AgentOptions.SubAgents
// is set.
type CallSubAgentTool struct {
	*BaseTool
	agent  *Agent
	client SubAgentClient
}

var _ BackgroundTool = (*CallSubAgentTool)(nil)

func newCallSubAgentTool(agent *Agent, client SubAgentClient) *CallSubAgentTool {
	return &CallSubAgentTool{
		BaseTool: &BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        CallSubAgentToolName,
			Description: utils.Ptr(callSubAgentDescription),
			Parameters: map[string]any{
				"type":     "object",
				"required": []string{"agent", "mode", "message"},
				"properties": map[string]any{
					"agent": map[string]any{"type": "string", "description": "Name of the sub-agent, exactly as listed."},
					"mode": map[string]any{
						"type":        "string",
						"enum":        []string{SubAgentModeSync, SubAgentModeAsync},
						"description": "sync waits for the answer; async returns at once and the answer arrives on its own.",
					},
					"message": map[string]any{"type": "string", "description": "The message for the sub-agent."},
					"thread_id": map[string]any{
						"type":        "string",
						"description": "Thread ID of an earlier call to follow up in. Leave empty to start a new thread.",
					},
				},
				"additionalProperties": false,
			},
		}}},
		agent:  agent,
		client: client,
	}
}

type callSubAgentArgs struct {
	Agent    string `json:"agent"`
	Mode     string `json:"mode"`
	Message  string `json:"message"`
	ThreadID string `json:"thread_id"`
}

// subAgentTaskPayload travels from an async call to AwaitTask, which under a
// durable runtime runs in another process.
type subAgentTaskPayload struct {
	Agent     string `json:"agent"`
	ThreadID  string `json:"thread_id"`
	GroupID   string `json:"group_id"`
	MessageID string `json:"message_id"`
	SenderID  string `json:"sender_id"`
	Message   string `json:"message"`
}

// Execute hands the message over: into the run already going on the
// sub-agent's thread, or as a new turn that it waits for (sync) or leaves for
// AwaitTask (async).
func (t *CallSubAgentTool) Execute(ctx context.Context, params *ToolCall) (*ToolCallResponse, error) {
	var args callSubAgentArgs
	if err := sonic.Unmarshal([]byte(params.Arguments), &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Message) == "" {
		return nil, fmt.Errorf("message is required")
	}
	if args.Mode != SubAgentModeSync && args.Mode != SubAgentModeAsync {
		return nil, fmt.Errorf("mode must be %q or %q", SubAgentModeSync, SubAgentModeAsync)
	}
	if err := t.checkListed(ctx, params, args.Agent); err != nil {
		return nil, err
	}

	// The message id is fixed by the call, so a retried call sends the same
	// message, and the client finds it on the thread rather than running it
	// twice; a new thread takes it as its id too.
	messageID := callSubAgentMessageID(params)
	threadID := messageID
	if args.ThreadID != "" {
		threadID = args.ThreadID
	}
	req := t.request(args.Agent, params, threadID, subAgentMessage(messageID, params.AgentName, args.Message))

	if args.ThreadID != "" {
		steered, err := t.client.SteerSubAgent(ctx, req)
		if err != nil {
			return nil, err
		}
		if steered {
			return textResponse(params, withSubAgentThreadID(fmt.Sprintf(
				"%s is still working on its current task, so this message was added to it; the answer to that task will cover it.", args.Agent), threadID)), nil
		}
	}

	if args.Mode == SubAgentModeSync {
		outcome, err := t.client.RunSubAgent(ctx, req)
		if err != nil {
			return nil, err
		}
		return textResponse(params, subAgentAnswer(args.Agent, threadID, outcome)), nil
	}

	payload, err := json.Marshal(subAgentTaskPayload{Agent: args.Agent, ThreadID: threadID, GroupID: params.NewThreadGroupID(), MessageID: messageID, SenderID: params.AgentName, Message: args.Message})
	if err != nil {
		return nil, err
	}
	resp := textResponse(params, withSubAgentThreadID(fmt.Sprintf(
		"Started %s. It is working in the background; its answer will arrive on its own when it is done, so carry on rather than waiting.", args.Agent), threadID))
	resp.TaskID = messageID
	resp.TaskPayload = payload
	return resp, nil
}

// AwaitTask runs an async call's sub-agent thread to its end and reports its
// answer.
func (t *CallSubAgentTool) AwaitTask(ctx context.Context, task BackgroundTaskRef, _ ProgressReporter) (BackgroundResult, error) {
	var payload subAgentTaskPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return BackgroundResult{}, fmt.Errorf("sub-agent task %s: reading its payload: %w", task.TaskID, err)
	}
	req := t.request(payload.Agent, &ToolCall{Namespace: task.Namespace, ThreadID: task.ThreadID, GroupID: payload.GroupID, SessionID: task.SessionID, RunContext: task.RunContext},
		payload.ThreadID, subAgentMessage(payload.MessageID, payload.SenderID, payload.Message))

	// Safe to retry: a wait retried after its worker died finds the turn the
	// first attempt started instead of starting it again. A follow-up that
	// raced past Execute's check joins the run already going.
	outcome, err := t.client.RunSubAgent(ctx, req)
	if err != nil {
		return BackgroundResult{}, fmt.Errorf("sub-agent task %s: %w", task.TaskID, err)
	}
	return BackgroundResult{Output: BackgroundText(subAgentAnswer(payload.Agent, payload.ThreadID, outcome))}, nil
}

// checkListed refuses a sub-agent the client would not list for this run.
func (t *CallSubAgentTool) checkListed(ctx context.Context, params *ToolCall, name string) error {
	listed, err := listSubAgentCatalog(ctx, t.client, SubAgentQuery{Caller: t.agent.Name, Namespace: params.Namespace, RunContext: params.RunContext})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(listed))
	for _, info := range listed {
		if info.Name == name {
			return nil
		}
		names = append(names, info.Name)
	}
	return fmt.Errorf("no sub-agent named %q; call one of: %s", name, strings.Join(names, ", "))
}

// request is a message for the named sub-agent on threadID: in the calling
// thread's namespace, in the group call names for new threads, parented to the
// calling thread, hidden from listing UIs, and sharing the caller's session
// for its attachments (not its history).
func (t *CallSubAgentTool) request(name string, call *ToolCall, threadID string, message history.Message) SubAgentRequest {
	return SubAgentRequest{
		Caller: t.agent,
		Name:   name,
		Input: &AgentInput{
			Namespace:      call.Namespace,
			GroupID:        call.NewThreadGroupID(),
			ParentThreadID: call.ThreadID,
			Hidden:         true,
			ThreadID:       threadID,
			SessionID:      call.SessionID,
			RunContext:     call.RunContext,
			// The thread's own channel, so a client can rejoin it and a
			// second message cannot start a second run on it.
			StreamID: StreamIDForThread(call.Namespace, threadID),
			Message:  message,
		},
	}
}

// subAgentAnswer is what the model reads of a sub-agent's turn.
func subAgentAnswer(name, threadID string, outcome AgentTaskOutcome) string {
	switch {
	case outcome.Joined:
		return withSubAgentThreadID(fmt.Sprintf(
			"%s was still working, so this message was added to its current task; its answer to that task will cover it.", name), threadID)
	case outcome.Output != nil && outcome.Output.Status == agentstate.RunStatusPaused:
		return withSubAgentThreadID(fmt.Sprintf(
			"%s stopped to ask for approval or input, which cannot be given to a sub-agent, so its task is unfinished.", name), threadID)
	}
	var text string
	if outcome.Output != nil {
		text = outcome.Output.Text()
	}
	return withSubAgentThreadID(text, threadID)
}

func subAgentMessage(id, senderID, text string) history.Message {
	return history.Message{
		ID:       id,
		SenderID: senderID,
		Messages: []responses.InputMessageUnion{{OfEasyInput: &responses.EasyMessage{
			Role:    constants.RoleUser,
			Content: responses.EasyInputContentUnion{OfString: &text},
		}}},
	}
}

func textResponse(params *ToolCall, text string) *ToolCallResponse {
	return &ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID:     params.ID,
		CallID: params.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(text)},
	}}
}

// withSubAgentThreadID adds the thread id the model sends a follow-up to.
func withSubAgentThreadID(text, threadID string) string {
	return text + fmt.Sprintf("\n---\nThread ID: %s", threadID)
}

// callSubAgentMessageID is an id fixed by the calling thread and tool call, so
// a retried call reuses it.
func callSubAgentMessageID(call *ToolCall) string {
	callID := call.CallID
	if callID == "" {
		callID = call.ID
	}
	if callID == "" {
		return uuid.NewString()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(call.Namespace+"\x00"+call.ThreadID+"\x00"+callID)).String()
}
