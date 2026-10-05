package agui

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// RunAgentInput is the canonical AG-UI request body. Matches the
// upstream `RunAgentInput` type byte-for-byte (camelCase field
// names, optional fields omitempty) so any AG-UI-compliant client
// (CopilotKit, raw fetch, etc.) can POST it as-is.
type RunAgentInput struct {
	ThreadID       string         `json:"threadId"`
	RunID          string         `json:"runId,omitempty"`
	State          any            `json:"state,omitempty"`
	Messages       []Message      `json:"messages"`
	Tools          []InputTool    `json:"tools,omitempty"`
	Context        []InputContext `json:"context,omitempty"`
	ForwardedProps any            `json:"forwardedProps,omitempty"`
	// Resume answers the interrupts that ended the run this one continues (AG-UI 1.0).
	Resume []ResumeEntry `json:"resume,omitempty"`
}

// Resume entry statuses (AG-UI 1.0).
const (
	ResumeResolved  = "resolved"
	ResumeCancelled = "cancelled"
)

// ResumeEntry answers one interrupt. InterruptID is the paused tool call's id.
// A resolved approval's payload is {"approved": bool}; a form's payload is the
// filled form; a URL elicitation needs no payload. Cancelled declines.
type ResumeEntry struct {
	InterruptID string          `json:"interruptId"`
	Status      string          `json:"status"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Metadata    map[string]any  `json:"metadata,omitempty"`
}

// decision maps a resume entry onto the approve/reject resolution the agent loop takes.
func (e ResumeEntry) decision() ApprovalDecision {
	d := ApprovalDecision{ToolCallID: e.InterruptID}
	if e.Status != ResumeResolved {
		return d
	}
	d.Approved = true
	var verdict struct {
		Approved *bool `json:"approved"`
	}
	var fields map[string]json.RawMessage
	if len(e.Payload) == 0 || string(e.Payload) == "null" {
		return d
	}
	if json.Unmarshal(e.Payload, &fields) == nil && json.Unmarshal(e.Payload, &verdict) == nil && verdict.Approved != nil {
		d.Approved = *verdict.Approved
		if len(fields) == 1 {
			// A bare verdict carries no data for the tool.
			return d
		}
	}
	d.Content = e.Payload
	return d
}

// InputTool is a client-defined frontend action. It becomes a client tool
// for the run (see agents.ClientToolDefinition): the model can call it, and
// the client runs it and returns the result. Server-side tools win name
// collisions.
type InputTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// InputContext is the AG-UI "additional grounding context" field —
// short string snippets the agent should treat as authoritative
// background (current page URL, selected text, etc.). The handler
// flattens these into the run's RunContext so prompt templates can
// reference them via {{Context.X}} macros.
type InputContext struct {
	Description string `json:"description"`
	Value       string `json:"value"`
}

// Validate sanity-checks the input shape before we burn an agent
// invocation on it. Returns a structured error so the handler can
// surface clean 400s.
//
// Validation rule: at least one of messages[] or resume[] must be
// non-empty. A resume-only POST is the canonical HITL resume shape —
// the client received an interrupted run, the user answered, and we
// POST back nothing but the answers.
func (in *RunAgentInput) Validate() error {
	if in == nil {
		return errors.New("agui: nil input")
	}
	if in.ThreadID == "" {
		return errors.New("agui: threadId is required")
	}
	for i, entry := range in.Resume {
		if entry.InterruptID == "" || (entry.Status != ResumeResolved && entry.Status != ResumeCancelled) {
			return fmt.Errorf("agui: resume[%d] needs an interruptId and a status of resolved or cancelled", i)
		}
	}
	if len(in.Messages) == 0 && len(in.Resume) == 0 {
		return errors.New("agui: at least one of messages or resume is required")
	}
	for i, m := range in.Messages {
		if m.Role == "" {
			return fmt.Errorf("agui: messages[%d].role is required", i)
		}
	}
	if _, err := in.SkillSelection(); err != nil {
		return err
	}
	_, err := in.MCPSelection()
	return err
}

// ApprovalDecision is a resume entry as the agent loop takes it: approve or
// reject one paused tool call. Content carries the answer to a data-carrying
// interrupt — the fields of a submitted form elicitation, matching the
// requestedSchema the pause advertised. It rides through to
// InterruptResolution.Content, which the agent loop hands to the resuming
// tool via ToolCall.ResumeMessages. Plain approvals leave it empty.
type ApprovalDecision struct {
	ToolCallID string
	Approved   bool
	Content    json.RawMessage
}

// ExtractApprovals returns the decisions carried by the run's AG-UI 1.0
// resume entries, one per answered interrupt.
func (in *RunAgentInput) ExtractApprovals() []ApprovalDecision {
	if in == nil || len(in.Resume) == 0 {
		return nil
	}
	decisions := make([]ApprovalDecision, 0, len(in.Resume))
	for _, entry := range in.Resume {
		if entry.InterruptID != "" {
			decisions = append(decisions, entry.decision())
		}
	}
	return decisions
}

// ApprovalsToMessage builds the SDK-shaped interrupt resolution message
// that the agent loop's ProcessIncomingMessages recognises, mapping each
// approve/reject decision onto a resolution action. Returns (nil, false)
// when there are no decisions so callers can skip the append cleanly.
func ApprovalsToMessage(decisions []ApprovalDecision) (*responses.FunctionCallInterruptResolutionMessage, bool) {
	if len(decisions) == 0 {
		return nil, false
	}
	msg := &responses.FunctionCallInterruptResolutionMessage{
		ID: "fcir_" + uuid.NewString(),
	}
	for _, d := range decisions {
		action := responses.InterruptActionReject
		if d.Approved {
			action = responses.InterruptActionApprove
		}
		msg.Resolutions = append(msg.Resolutions, responses.InterruptResolution{
			CallID: d.ToolCallID,
			Action: action,
			// Only an approval carries data. A rejected form has no answer to
			// deliver, and passing one through would hand the resuming tool
			// content the user declined to submit.
			Content: contentFor(action, d.Content),
		})
	}
	return msg, true
}

func contentFor(action string, content json.RawMessage) json.RawMessage {
	if action != responses.InterruptActionApprove {
		return nil
	}
	return content
}

// NewTurnSDKMessages converts only this turn's NEW messages (plus any
// approval decisions) into the SDK's InputMessageUnion list.
//
// AG-UI clients POST the full conversation on every turn, but the
// agent persists thread history itself and re-appends everything it
// is handed — forwarding the whole list would duplicate prior turns
// in the thread. The new turn is the trailing contiguous block of
// user/system/developer messages after the last assistant or tool
// message (everything at or before that point is server-side history
// the client is echoing back).
//
// Handlers use this by default; WithFullHistory switches them to
// ToSDKMessages for agents configured without persistence.
//
// @hastekit/copilotkit mirrors this rule (newTurnOf in
// packages/copilotkit/src/agent.ts) to send only the new turn; change both.
func (in *RunAgentInput) NewTurnSDKMessages() []responses.InputMessageUnion {
	// A client that just ran its own tools posts their results as trailing
	// tool messages (CopilotKit's follow-up run). Those results are the turn.
	if n := len(in.Messages); n > 0 && in.Messages[n-1].Role == RoleTool {
		start := n
		for start > 0 && in.Messages[start-1].Role == RoleTool {
			start--
		}
		return in.toSDKMessages(in.Messages[start:])
	}
	start := len(in.Messages)
	for start > 0 {
		switch in.Messages[start-1].Role {
		case RoleUser, RoleSystem, RoleDeveloper:
			start--
		default:
			return in.toSDKMessages(in.Messages[start:])
		}
	}
	return in.toSDKMessages(in.Messages)
}

// ToSDKMessages converts the full AG-UI message list into the agent
// SDK's InputMessageUnion list. Conversions:
//
//   - user/system/developer messages → InputMessage with input_text
//   - assistant messages with toolCalls → OutputMessage (text) +
//     one FunctionCallMessage per tool call
//   - assistant messages without toolCalls → OutputMessage
//   - tool messages → FunctionCallOutputMessage
//
// Unknown roles are dropped with no error — strict-mode would be a
// poor default given the spec lets clients invent custom roles.
//
// If the run carries resume entries, a single
// FunctionCallInterruptResolutionMessage is prepended so the agent's
// next iteration drains it via ProcessIncomingMessages and
// transitions out of StepAwaitApproval. Resolutions always go first
// in the list — the SDK reads them on iteration boundaries before
// any LLM call, and ordering them ahead of any new user messages
// matches the user's mental model ("I resolved this, then asked
// something else").
func (in *RunAgentInput) ToSDKMessages() []responses.InputMessageUnion {
	return in.toSDKMessages(in.Messages)
}

func (in *RunAgentInput) toSDKMessages(msgs []Message) []responses.InputMessageUnion {
	approvals := in.ExtractApprovals()
	out := make([]responses.InputMessageUnion, 0, len(msgs)+1)
	if approval, ok := ApprovalsToMessage(approvals); ok {
		out = append(out, responses.InputMessageUnion{
			OfFunctionCallInterruptResolution: approval,
		})
	}
	// A resumed call's result comes from the agent running it. Some clients
	// (CopilotKit) also add a tool message echoing the resume payload; it is
	// not the tool's output, so it is dropped rather than stored beside it.
	resumed := map[string]bool{}
	for _, d := range approvals {
		resumed[d.ToolCallID] = true
	}
	for _, m := range msgs {
		if m.Role == RoleTool && resumed[m.ToolCallID] {
			continue
		}
		switch m.Role {
		case RoleUser, RoleSystem, RoleDeveloper:
			out = append(out, responses.InputMessageUnion{
				OfInputMessage: &responses.InputMessage{
					ID:      normalizeMessageID(m.ID),
					Role:    constants.Role(m.Role),
					Content: messageContent(m),
				},
			})

		case RoleAssistant:
			if m.Content != "" {
				out = append(out, responses.InputMessageUnion{
					OfOutputMessage: &responses.OutputMessage{
						ID:   normalizeMessageID(m.ID),
						Role: constants.Role(m.Role),
						Content: &responses.OutputContent{
							{OfOutputText: &responses.OutputTextContent{
								Text:        m.Content,
								Annotations: []responses.Annotation{},
							}},
						},
					},
				})
			}
			for _, tc := range m.ToolCalls {
				out = append(out, responses.InputMessageUnion{
					OfFunctionCall: &responses.FunctionCallMessage{
						ID:        ensureFunctionCallID(tc.ID),
						CallID:    tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}

		case RoleTool:
			out = append(out, responses.InputMessageUnion{
				OfFunctionCallOutput: &responses.FunctionCallOutputMessage{
					ID:     m.ID,
					CallID: m.ToolCallID,
					Output: responses.FunctionCallOutputContentUnion{
						OfString: ptr(m.Content),
					},
				},
			})
		}
	}
	return out
}

// normalizeMessageID coerces an AG-UI message id into the provider's
// message-id convention: a "msg" prefix, which the OpenAI Responses
// API requires on message items ("Invalid 'input[0].id': … Expected
// an ID that begins with 'msg'"). AG-UI clients assign bare UUIDs to
// messages they originate locally (CopilotKit's HttpAgent does this),
// and forwarding those verbatim as provider message ids is rejected.
// An empty id mints a fresh one; an already-prefixed id passes
// through; anything else is prefixed so the client's id stays
// correlatable.
//
// @hastekit/copilotkit mirrors this rule (serverIdOf in
// packages/copilotkit/src/agent.ts) to recognise its own echoed turns.
func normalizeMessageID(id string) string {
	switch {
	case id == "":
		return "msg_" + uuid.NewString()
	case strings.HasPrefix(id, "msg"):
		return id
	default:
		return "msg_" + id
	}
}

func ensureFunctionCallID(id string) string {
	if id != "" {
		return id
	}
	return "fc_" + uuid.NewString()
}

func ptr[T any](v T) *T { return &v }

// SkillSelection reads the SDK extension forwardedProps.skills. Clients must
// resend their selection on every run, including approval resumes.
func (in *RunAgentInput) SkillSelection() (agents.SkillSelection, error) {
	var selection agents.SkillSelection
	fp, ok := in.ForwardedProps.(map[string]any)
	if !ok || fp["skills"] == nil {
		return selection, nil
	}
	data, err := json.Marshal(fp["skills"])
	if err == nil {
		err = json.Unmarshal(data, &selection)
	}
	if err != nil {
		return selection, fmt.Errorf("agui: invalid forwardedProps.skills: %w", err)
	}
	return selection, nil
}

// MCPSelection reads per-run server and tool selection from forwardedProps.mcp.
func (in *RunAgentInput) MCPSelection() (agents.MCPSelection, error) {
	var selection agents.MCPSelection
	props, ok := in.ForwardedProps.(map[string]any)
	if !ok || props["mcp"] == nil {
		return selection, nil
	}
	data, err := json.Marshal(props["mcp"])
	if err == nil {
		err = json.Unmarshal(data, &selection)
	}
	if err != nil {
		return selection, fmt.Errorf("agui: invalid forwardedProps.mcp: %w", err)
	}
	return selection, nil
}

// ClientTools converts the run's frontend tools into client tool definitions.
func (in *RunAgentInput) ClientTools() []agents.ClientToolDefinition {
	if len(in.Tools) == 0 {
		return nil
	}
	out := make([]agents.ClientToolDefinition, 0, len(in.Tools))
	for _, tool := range in.Tools {
		out = append(out, agents.ClientToolDefinition{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	return out
}
