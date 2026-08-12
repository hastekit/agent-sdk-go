package agui

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
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
}

// InputTool is a client-defined frontend action. We accept the shape
// for spec compliance but don't dispatch client-side tools in v1 —
// the agent's configured server-side tools take precedence. Future
// work: register these as ephemeral tools for the run.
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
// Validation rule: at least one of messages[] or approvals must be
// non-empty. An approval-only POST is the canonical HITL resume
// shape — the client received a paused run, the user clicked
// approve/reject, and we POST back nothing but the decisions.
func (in *RunAgentInput) Validate() error {
	if in == nil {
		return errors.New("agui: nil input")
	}
	if in.ThreadID == "" {
		return errors.New("agui: threadId is required")
	}
	approvals := in.ExtractApprovals()
	if len(in.Messages) == 0 && len(approvals) == 0 {
		return errors.New("agui: at least one of messages or forwardedProps.command.resume is required")
	}
	for i, m := range in.Messages {
		if m.Role == "" {
			return fmt.Errorf("agui: messages[%d].role is required", i)
		}
	}
	return nil
}

// ApprovalDecision is one entry in forwardedProps.command.resume —
// the AG-UI-side expression of a human-in-the-loop decision for a
// paused tool call. We split the protocol-side shape (a forwardedProps
// extension) from the SDK-side shape (FunctionCallInterruptResolutionMessage
// with action verbs) so the wire contract stays AG-UI-native while the
// agent loop sees the form it already understands.
//
// Content carries the answer to a data-carrying interrupt — the fields of a
// submitted form elicitation, matching the requestedSchema the pause
// advertised. It rides through to InterruptResolution.Content, which the
// agent loop hands to the resuming tool via ToolCall.ResumeMessages. Plain
// approvals leave it empty.
type ApprovalDecision struct {
	ToolCallID string          `json:"toolCallId"`
	Approved   bool            `json:"approved"`
	Content    json.RawMessage `json:"content,omitempty"`

	// Remember makes the verdict standing rather than one-off: the tool is
	// added to the thread's always-allow or always-deny list and is not asked
	// about again. It is the "don't ask me again" checkbox on the prompt.
	Remember bool `json:"remember,omitempty"`
}

// UnmarshalJSON resolves what "approved" means across the shapes clients
// actually send, because the zero value of a bool is a rejection and
// silently discarding a submitted form is the worst possible default.
//
// Precedence:
//
//  1. An explicit "action" verb wins. Both this SDK's approve/reject and
//     MCP's elicitation verbs (accept/decline/cancel) are accepted, so a
//     frontend can forward an MCP-shaped answer unchanged.
//  2. An explicit "approved" boolean.
//  3. Otherwise, a decision carrying content is a submission, and therefore
//     an approval. A form arriving with no verdict field means the user
//     filled it in and pressed submit.
//  4. Otherwise, a rejection.
func (d *ApprovalDecision) UnmarshalJSON(b []byte) error {
	var raw struct {
		ToolCallID string          `json:"toolCallId"`
		Approved   *bool           `json:"approved"`
		Action     string          `json:"action"`
		Content    json.RawMessage `json:"content"`
		Remember   bool            `json:"remember"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}

	d.ToolCallID = raw.ToolCallID
	d.Remember = raw.Remember
	d.Content = nil
	// A literal null is not content. Treat it as absent so it cannot flip a
	// missing verdict into an approval below.
	if len(raw.Content) > 0 && string(raw.Content) != "null" {
		d.Content = raw.Content
	}

	switch strings.ToLower(strings.TrimSpace(raw.Action)) {
	case "approve", "accept":
		d.Approved = true
	case "reject", "decline", "cancel":
		d.Approved = false
	default:
		switch {
		case raw.Approved != nil:
			d.Approved = *raw.Approved
		case len(d.Content) > 0:
			d.Approved = true
		default:
			d.Approved = false
		}
	}
	return nil
}

// ExtractApprovals returns the parsed approval decisions from
// forwardedProps, accepting both the CopilotKit-canonical
// "command.resume" shape and a flat hastekitApprovals alias.
//
// Canonical (matches CopilotKit useInterrupt's resolve payload):
//
//	{
//	  "forwardedProps": {
//	    "command": {
//	      "resume": {
//	        "decisions": [
//	          { "toolCallId": "call_xyz", "approved": true }
//	        ]
//	      },
//	      "interruptEvent": { ... }   // optional — useInterrupt echoes
//	                                  // the original event value here;
//	                                  // we ignore it (server has the
//	                                  // saved RunState already).
//	    }
//	  }
//	}
//
// Alias (simpler clients):
//
//	{ "forwardedProps": { "hastekitApprovals": [ {…} ] } }
//
// Returns an empty slice (not an error) for any malformed shape —
// approvals are a hint, not a load-bearing contract.
func (in *RunAgentInput) ExtractApprovals() []ApprovalDecision {
	if in == nil {
		return nil
	}
	fp, ok := in.ForwardedProps.(map[string]any)
	if !ok {
		return nil
	}
	if raw := lookupCanonicalResume(fp); raw != nil {
		return decodeDecisions(raw)
	}
	if raw, ok := fp["hastekitApprovals"]; ok && raw != nil {
		return decodeDecisions(raw)
	}
	return nil
}

// ExtractPermissionMode reads the tool gating the client asked for from
// forwardedProps:
//
//	{ "forwardedProps": { "permissionMode": "allow_all" } }
//
// Empty — absent, unreadable, or a mode this SDK doesn't know — means the
// default, which is the gating mode. An unrecognised value is not an error a
// run should fail on, but it must not be taken as permission either, so it
// reads as "ask me".
func (in *RunAgentInput) ExtractPermissionMode() agents.PermissionMode {
	if in == nil {
		return ""
	}
	fp, ok := in.ForwardedProps.(map[string]any)
	if !ok {
		return ""
	}
	mode, _ := fp["permissionMode"].(string)

	switch agents.PermissionMode(strings.TrimSpace(mode)) {
	case agents.PermissionModeAllowAll:
		return agents.PermissionModeAllowAll
	case agents.PermissionModeDefault:
		return agents.PermissionModeDefault
	default:
		return ""
	}
}

// lookupCanonicalResume drills into forwardedProps.command.resume
// and returns the .decisions array (or the raw .resume value when
// it's already an array — some clients flatten the structure).
func lookupCanonicalResume(fp map[string]any) any {
	command, ok := fp["command"].(map[string]any)
	if !ok {
		return nil
	}
	resume := command["resume"]
	if resume == nil {
		return nil
	}
	if m, ok := resume.(map[string]any); ok {
		if decisions, ok := m["decisions"]; ok {
			return decisions
		}
	}
	// Already an array — accept the bare form too.
	if _, ok := resume.([]any); ok {
		return resume
	}
	return nil
}

func decodeDecisions(raw any) []ApprovalDecision {
	buf, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []ApprovalDecision
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil
	}
	cleaned := out[:0]
	for _, d := range out {
		if d.ToolCallID == "" {
			continue
		}
		cleaned = append(cleaned, d)
	}
	return cleaned
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
			CallID:         d.ToolCallID,
			Action:         action,
			RememberAction: d.Remember,
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
func (in *RunAgentInput) NewTurnSDKMessages() []responses.InputMessageUnion {
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
// If approval decisions are present in forwardedProps, a single
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
	for _, m := range msgs {
		switch m.Role {
		case RoleUser, RoleSystem, RoleDeveloper:
			out = append(out, responses.InputMessageUnion{
				OfInputMessage: &responses.InputMessage{
					ID:   normalizeMessageID(m.ID),
					Role: constants.Role(m.Role),
					Content: responses.InputContent{
						{OfInputText: &responses.InputTextContent{
							Text: m.Content,
						}},
					},
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
