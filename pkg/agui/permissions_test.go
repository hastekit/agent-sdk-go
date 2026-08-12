package agui

import (
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mode the client picked reaches the run.
func TestExtractPermissionMode(t *testing.T) {
	allowAll := &RunAgentInput{ForwardedProps: map[string]any{"permissionMode": "allow_all"}}
	assert.Equal(t, agents.PermissionModeAllowAll, allowAll.ExtractPermissionMode())

	explicit := &RunAgentInput{ForwardedProps: map[string]any{"permissionMode": "default"}}
	assert.Equal(t, agents.PermissionModeDefault, explicit.ExtractPermissionMode())
}

// Anything the SDK can't read as a mode leaves the field empty, which the
// agent loop treats as the gating default. A typo must not become permission.
func TestExtractPermissionMode_UnreadableMeansAsk(t *testing.T) {
	for name, input := range map[string]*RunAgentInput{
		"absent":        {ForwardedProps: map[string]any{}},
		"no props":      {},
		"props garbage": {ForwardedProps: "garbage"},
		"unknown mode":  {ForwardedProps: map[string]any{"permissionMode": "allow_everything"}},
		"wrong type":    {ForwardedProps: map[string]any{"permissionMode": true}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, agents.PermissionMode(""), input.ExtractPermissionMode())
		})
	}
}

// A remembered decision survives the trip from the AG-UI shape to the SDK's.
func TestApprovalsToMessage_CarriesRemember(t *testing.T) {
	msg, ok := ApprovalsToMessage([]ApprovalDecision{
		{ToolCallID: "call_1", Approved: true, Remember: true},
		{ToolCallID: "call_2", Approved: false},
	})
	require.True(t, ok)
	require.Len(t, msg.Resolutions, 2)

	assert.True(t, msg.Resolutions[0].RememberAction)
	assert.Equal(t, responses.InterruptActionApprove, msg.Resolutions[0].Action)
	assert.False(t, msg.Resolutions[1].RememberAction, "an ordinary answer stays one-off")
}

// The client is told which pauses can be remembered, so it only offers the
// checkbox where checking it would do something.
func TestProjectInterrupts_AdvertisesCanRemember(t *testing.T) {
	projected := projectInterrupts([]responses.Interrupt{
		{
			FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_1", Name: "send_email"},
			Mode:                responses.InterruptModeApproval,
		},
		{
			FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_2", Name: "book"},
			Mode:                responses.InterruptModeForm,
		},
		{
			// No tool name: nothing to record a standing decision against.
			FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_3"},
			Mode:                responses.InterruptModeApproval,
		},
	})
	require.Len(t, projected, 3)

	assert.Equal(t, true, projected[0]["canRemember"])
	assert.NotContains(t, projected[1], "canRemember", "a form answers this call, not every call")
	assert.NotContains(t, projected[2], "canRemember")
}
