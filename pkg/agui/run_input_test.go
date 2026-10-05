package agui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	assert.Error(t, (&RunAgentInput{}).Validate())
	assert.Error(t, (&RunAgentInput{ThreadID: "t"}).Validate())
	assert.Error(t, (&RunAgentInput{ThreadID: "t", Messages: []Message{{Content: "no role"}}}).Validate())

	assert.NoError(t, (&RunAgentInput{
		ThreadID: "t",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}).Validate())

	// A resume-only POST (the HITL resume shape) is valid without messages.
	assert.NoError(t, (&RunAgentInput{
		ThreadID: "t",
		Resume:   []ResumeEntry{{InterruptID: "call_1", Status: ResumeResolved, Payload: json.RawMessage(`{"approved":true}`)}},
	}).Validate())
}

func TestExtractApprovals(t *testing.T) {
	in := &RunAgentInput{Resume: []ResumeEntry{
		{InterruptID: "call_1", Status: ResumeResolved, Payload: json.RawMessage(`{"approved":true}`)},
		{InterruptID: "call_2", Status: ResumeResolved, Payload: json.RawMessage(`{"approved":false}`)},
		{Status: ResumeResolved}, // missing id — dropped
	}}
	decisions := in.ExtractApprovals()
	require.Len(t, decisions, 2)
	assert.Equal(t, ApprovalDecision{ToolCallID: "call_1", Approved: true}, decisions[0])
	assert.Equal(t, ApprovalDecision{ToolCallID: "call_2", Approved: false}, decisions[1])

	assert.Empty(t, (&RunAgentInput{}).ExtractApprovals())
	assert.Empty(t, (&RunAgentInput{ForwardedProps: map[string]any{
		"hastekitApprovals": []any{map[string]any{"toolCallId": "call_3", "approved": true}},
	}}).ExtractApprovals(), "answers come only from resume entries")
}

func TestNewTurnSDKMessagesExtractsTrailingTurn(t *testing.T) {
	in := &RunAgentInput{
		ThreadID: "t",
		Messages: []Message{
			{Role: RoleUser, Content: "first question"},
			{Role: RoleAssistant, Content: "first answer"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: "{}"}}}},
			{Role: RoleTool, ToolCallID: "call_1", Content: "result"},
			{Role: RoleUser, Content: "follow-up"},
			{Role: RoleUser, Content: "and one more thing"},
		},
	}

	out := in.NewTurnSDKMessages()
	require.Len(t, out, 2)
	require.NotNil(t, out[0].OfInputMessage)
	assert.Equal(t, "follow-up", out[0].OfInputMessage.Content[0].OfInputText.Text)
	require.NotNil(t, out[1].OfInputMessage)
	assert.Equal(t, "and one more thing", out[1].OfInputMessage.Content[0].OfInputText.Text)
}

func TestNewTurnSDKMessagesFirstTurnTakesAll(t *testing.T) {
	in := &RunAgentInput{
		ThreadID: "t",
		Messages: []Message{
			{Role: RoleSystem, Content: "be helpful"},
			{Role: RoleUser, Content: "hi"},
		},
	}
	assert.Len(t, in.NewTurnSDKMessages(), 2)
}

func TestNewTurnSDKMessagesApprovalOnly(t *testing.T) {
	in := &RunAgentInput{
		ThreadID: "t",
		Messages: []Message{
			{Role: RoleUser, Content: "old"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Type: "function"}}},
		},
		Resume: []ResumeEntry{{InterruptID: "call_1", Status: ResumeResolved, Payload: json.RawMessage(`{"approved":true}`)}},
	}

	out := in.NewTurnSDKMessages()
	// History tail is an assistant message → no new turn messages;
	// only the resolution is forwarded, and it goes first.
	require.Len(t, out, 1)
	require.NotNil(t, out[0].OfFunctionCallInterruptResolution)
	res := out[0].OfFunctionCallInterruptResolution.Resolutions
	require.Len(t, res, 1)
	assert.Equal(t, "call_1", res[0].CallID)
	assert.Equal(t, "approve", res[0].Action)
}

func TestMessageIDsAreProviderPrefixed(t *testing.T) {
	// CopilotKit's HttpAgent assigns bare UUIDs; the OpenAI Responses
	// API rejects message ids that don't begin with "msg". The
	// conversion must coerce them.
	in := &RunAgentInput{
		ThreadID: "t",
		Messages: []Message{
			{ID: "e3e5ee9f-49b0-439b-8ad5-730a4d1a1dc9", Role: RoleUser, Content: "hi"},
			{ID: "", Role: RoleUser, Content: "no id"},
			{ID: "msg_keepme", Role: RoleAssistant, Content: "kept"},
		},
	}

	out := in.ToSDKMessages()
	require.Len(t, out, 3)

	// Bare UUID → prefixed, preserving the original for correlation.
	require.NotNil(t, out[0].OfInputMessage)
	assert.Equal(t, "msg_e3e5ee9f-49b0-439b-8ad5-730a4d1a1dc9", out[0].OfInputMessage.ID)

	// Empty → freshly minted, still prefixed.
	require.NotNil(t, out[1].OfInputMessage)
	assert.True(t, strings.HasPrefix(out[1].OfInputMessage.ID, "msg_"))

	// Already-prefixed assistant id passes through untouched.
	require.NotNil(t, out[2].OfOutputMessage)
	assert.Equal(t, "msg_keepme", out[2].OfOutputMessage.ID)
}

func TestToSDKMessagesFullConversion(t *testing.T) {
	in := &RunAgentInput{
		ThreadID: "t",
		Messages: []Message{
			{Role: RoleUser, Content: "q"},
			{Role: RoleAssistant, Content: "a", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: "{}"}}}},
			{Role: RoleTool, ToolCallID: "call_1", Content: "result"},
			{Role: "made-up-role", Content: "dropped silently"},
		},
	}

	out := in.ToSDKMessages()
	require.Len(t, out, 4)
	assert.NotNil(t, out[0].OfInputMessage)
	assert.NotNil(t, out[1].OfOutputMessage)
	require.NotNil(t, out[2].OfFunctionCall)
	assert.Equal(t, "call_1", out[2].OfFunctionCall.CallID)
	require.NotNil(t, out[3].OfFunctionCallOutput)
	assert.Equal(t, "result", *out[3].OfFunctionCallOutput.Output.OfString)
}

// A form elicitation is resolved by submitting its fields. A payload with no
// verdict must not be read as a rejection — that would discard what the user
// typed and resume the tool with nothing.
func TestFormSubmissionIsAnApproval(t *testing.T) {
	in := &RunAgentInput{Resume: []ResumeEntry{{
		InterruptID: "call_1", Status: ResumeResolved,
		Payload: json.RawMessage(`{"passport_no":"X1234567"}`),
	}}}

	decisions := in.ExtractApprovals()
	require.Len(t, decisions, 1)
	assert.True(t, decisions[0].Approved)
	assert.JSONEq(t, `{"passport_no":"X1234567"}`, string(decisions[0].Content))

	msg, ok := ApprovalsToMessage(decisions)
	require.True(t, ok)
	require.Len(t, msg.Resolutions, 1)
	assert.Equal(t, responses.InterruptActionApprove, msg.Resolutions[0].Action)
	assert.JSONEq(t, `{"passport_no":"X1234567"}`, string(msg.Resolutions[0].Content))
}

// An explicit verdict always wins over the content heuristic, and a declined
// or cancelled interrupt must not deliver content the user chose not to submit.
func TestRejectedDecisionDropsContent(t *testing.T) {
	in := &RunAgentInput{Resume: []ResumeEntry{
		{InterruptID: "call_1", Status: ResumeResolved, Payload: json.RawMessage(`{"approved":false,"passport_no":"X1234567"}`)},
		{InterruptID: "call_2", Status: ResumeCancelled, Payload: json.RawMessage(`{"passport_no":"X1234567"}`)},
	}}

	decisions := in.ExtractApprovals()
	require.Len(t, decisions, 2)
	msg, ok := ApprovalsToMessage(decisions)
	require.True(t, ok)
	for _, resolution := range msg.Resolutions {
		assert.Equal(t, responses.InterruptActionReject, resolution.Action, resolution.CallID)
		assert.Empty(t, resolution.Content, resolution.CallID)
	}
}

// HastekitAgent (packages/copilotkit) trims the run body down to the new turn
// instead of re-posting the whole thread on every message (newTurnOf,
// mirroring NewTurnSDKMessages). That is only
// safe because the rule is idempotent: reapplying it to an already-trimmed
// list has to select the same messages, or the client and server would
// disagree about what the turn is.
//
// This pins the property for the shapes a client actually posts. If
// NewTurnSDKMessages ever stops being idempotent, the trimming in the UI
// has to go with it.
func TestNewTurnSDKMessagesIsIdempotent(t *testing.T) {
	toolCall := []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: "{}"}}}

	cases := map[string][]Message{
		"follow-up turn": {
			{ID: "m1", Role: RoleUser, Content: "first question"},
			{ID: "m2", Role: RoleAssistant, Content: "first answer"},
			{ID: "m3", Role: RoleUser, Content: "follow-up"},
		},
		"multi-message turn": {
			{ID: "m1", Role: RoleUser, Content: "old"},
			{ID: "m2", Role: RoleAssistant, Content: "answer"},
			{ID: "m3", Role: RoleSystem, Content: "be brief"},
			{ID: "m3", Role: RoleUser, Content: "follow-up"},
		},
		"first turn": {
			{ID: "m1", Role: RoleSystem, Content: "be helpful"},
			{ID: "m2", Role: RoleUser, Content: "hi"},
		},
		"after a tool call": {
			{ID: "m1", Role: RoleUser, Content: "old"},
			{ID: "m2", Role: RoleAssistant, ToolCalls: toolCall},
			{ID: "m3", Role: RoleTool, ToolCallID: "call_1", Content: "result"},
			{ID: "m4", Role: RoleUser, Content: "next"},
		},
		"approval resume, no new text": {
			{ID: "m1", Role: RoleUser, Content: "old"},
			{ID: "m2", Role: RoleAssistant, ToolCalls: toolCall},
		},
		"empty": {},
	}

	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			full := &RunAgentInput{ThreadID: "t", Messages: msgs}

			// What the client keeps is the same trailing block the server
			// selects, so re-selecting from it must not move the boundary.
			trimmed := &RunAgentInput{ThreadID: "t", Messages: newTurnOf(msgs)}

			assert.Equal(t, full.NewTurnSDKMessages(), trimmed.NewTurnSDKMessages())
		})
	}
}

// newTurnOf is the Go twin of the UI's trimming, kept next to the test that
// uses it so the two stay comparable by inspection.
func newTurnOf(msgs []Message) []Message {
	start := len(msgs)
	for start > 0 {
		switch msgs[start-1].Role {
		case RoleUser, RoleSystem, RoleDeveloper:
			start--
		default:
			return msgs[start:]
		}
	}
	return msgs
}
