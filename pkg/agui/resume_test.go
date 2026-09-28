package agui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResumeEntriesBecomeDecisions(t *testing.T) {
	in := RunAgentInput{ThreadID: "t", Resume: []ResumeEntry{
		{InterruptID: "approve", Status: ResumeResolved, Payload: json.RawMessage(`{"approved":true}`)},
		{InterruptID: "deny", Status: ResumeResolved, Payload: json.RawMessage(`{"approved":false}`)},
		{InterruptID: "cancel", Status: ResumeCancelled},
		{InterruptID: "form", Status: ResumeResolved, Payload: json.RawMessage(`{"passenger":"Ada"}`)},
		{InterruptID: "url", Status: ResumeResolved},
	}}
	require.NoError(t, in.Validate())
	decisions := map[string]ApprovalDecision{}
	for _, d := range in.ExtractApprovals() {
		decisions[d.ToolCallID] = d
	}
	assert.Equal(t, ApprovalDecision{ToolCallID: "approve", Approved: true}, decisions["approve"])
	assert.Equal(t, ApprovalDecision{ToolCallID: "deny"}, decisions["deny"])
	assert.Equal(t, ApprovalDecision{ToolCallID: "cancel"}, decisions["cancel"])
	assert.JSONEq(t, `{"passenger":"Ada"}`, string(decisions["form"].Content))
	assert.True(t, decisions["form"].Approved)
	assert.Equal(t, ApprovalDecision{ToolCallID: "url", Approved: true}, decisions["url"])

	// The pre-1.0 forwardedProps.command.resume form is not an answer, and
	// malformed entries are rejected.
	legacy := RunAgentInput{ThreadID: "t", ForwardedProps: map[string]any{"command": map[string]any{"resume": map[string]any{"decisions": []any{map[string]any{"toolCallId": "legacy", "approved": true}}}}}}
	assert.Empty(t, legacy.ExtractApprovals())
	require.Error(t, legacy.Validate(), "with no messages and no resume there is nothing to run")
	bad := RunAgentInput{ThreadID: "t", Resume: []ResumeEntry{{InterruptID: "x", Status: "approved"}}}
	require.Error(t, bad.Validate())

	// A tool message echoing a resumed call is dropped; the turn is just the resolution.
	in.Messages = []Message{{ID: "t1", Role: RoleTool, ToolCallID: "approve", Content: `{"approved":true}`}}
	turn := in.NewTurnSDKMessages()
	require.Len(t, turn, 1)
	require.NotNil(t, turn[0].OfFunctionCallInterruptResolution)
}

// CopilotKit's useInterrupt resumes a tool_call interrupt with a resume entry
// and also a tool message echoing the payload. The approved tool must still run
// and its real output reach the model; the echo is not stored as the result.
func TestApprovalResumedLikeCopilotKit(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{
		{
			chunks:   []*responses.ResponseChunk{functionCallAdded("item-1", "call-1", "delete_user"), argsDelta("item-1", `{"user_id":"123"}`)},
			response: toolCallResponse("call-1", "delete_user", `{"user_id":"123"}`),
		},
		{response: assistantTextResponse("User 123 deleted.")},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "UserManager",
		Tools: []agents.Tool{newApprovalTool("delete_user", "deleted user 123")},
	}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"UserManager": agent}))
	defer server.Close()

	frames := postRun(t, server, "UserManager", RunAgentInput{
		ThreadID: "thread-ck",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "delete user 123"}},
	})
	finished, ok := findFrame(frames, "RUN_FINISHED")
	require.True(t, ok)
	outcome := finished.data["outcome"].(map[string]any)
	require.Equal(t, "interrupt", outcome["type"])
	interrupt := outcome["interrupts"].([]any)[0].(map[string]any)
	require.Equal(t, "call-1", interrupt["id"])
	require.Equal(t, "tool_call", interrupt["reason"])

	frames = postRun(t, server, "UserManager", RunAgentInput{
		ThreadID: "thread-ck",
		Resume:   []ResumeEntry{{InterruptID: "call-1", Status: ResumeResolved, Payload: json.RawMessage(`{"approved":true}`)}},
		Messages: []Message{
			{ID: "u1", Role: RoleUser, Content: "delete user 123"},
			{ID: "a1", Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Function: ToolCallFunction{Name: "delete_user", Arguments: `{"user_id":"123"}`}}}},
			{ID: "t1", Role: RoleTool, ToolCallID: "call-1", Content: `{"approved":true}`},
		},
	})
	result, ok := findFrame(frames, "TOOL_CALL_RESULT")
	require.True(t, ok)
	assert.Equal(t, "deleted user 123", result.data["content"])
	finished, ok = findFrame(frames, "RUN_FINISHED")
	require.True(t, ok)
	assert.Equal(t, "success", finished.data["outcome"].(map[string]any)["type"])
}
