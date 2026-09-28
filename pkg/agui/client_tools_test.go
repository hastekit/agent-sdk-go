package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var selectionInputTool = []InputTool{{
	Name:        "get_selection",
	Description: "Read the text the user has selected in the page.",
	Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
}}

// A CopilotKit-style frontend tool: the run pauses on the call, the client runs
// the tool and posts a follow-up whose trailing tool message carries the result.
func TestClientToolRoundTripLikeCopilotKit(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{
		{
			chunks:   []*responses.ResponseChunk{functionCallAdded("item-1", "call-1", "get_selection"), argsDelta("item-1", `{}`)},
			response: toolCallResponse("call-1", "get_selection", `{}`),
		},
		{
			chunks:   []*responses.ResponseChunk{messageAdded("msg_1"), textDelta("msg_1", "Revenue grew."), messageDone("msg_1")},
			response: assistantTextResponse("Revenue grew."),
		},
	}}
	// CopilotKit answers only after the run has paused, so there is no point waiting.
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Reader", ClientTools: agents.ClientToolOptions{Timeout: -1}}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"Reader": agent}))
	defer server.Close()

	frames := postRun(t, server, "Reader", RunAgentInput{
		ThreadID: "thread-client", Tools: selectionInputTool,
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "summarize my selection"}},
	})
	start, ok := findFrame(frames, "TOOL_CALL_START")
	require.True(t, ok)
	assert.Equal(t, "get_selection", start.data["toolCallName"])
	// AG-UI 1.0: a successful run that leaves the call unanswered for the
	// client. No person has to answer, so there is no interrupt, and state is
	// untouched. The outcome does not name the call: pre-1.0 clients reject it.
	finished, ok := findFrame(frames, "RUN_FINISHED")
	require.True(t, ok)
	assert.Equal(t, map[string]any{"type": "success"}, finished.data["outcome"])
	_, answered := findFrame(frames, "TOOL_CALL_RESULT")
	assert.False(t, answered)
	_, snapshotted := findFrame(frames, "STATE_SNAPSHOT")
	assert.False(t, snapshotted)

	frames = postRun(t, server, "Reader", RunAgentInput{
		ThreadID: "thread-client", Tools: selectionInputTool,
		Messages: []Message{
			{ID: "u1", Role: RoleUser, Content: "summarize my selection"},
			{ID: "a1", Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Function: ToolCallFunction{Name: "get_selection", Arguments: "{}"}}}},
			{ID: "t1", Role: RoleTool, ToolCallID: "call-1", Content: "Quarterly revenue grew 12%"},
		},
	})
	result, ok := findFrame(frames, "TOOL_CALL_RESULT")
	require.True(t, ok)
	assert.Equal(t, "Quarterly revenue grew 12%", result.data["content"])
	finished, ok = findFrame(frames, "RUN_FINISHED")
	require.True(t, ok)
	assert.Equal(t, map[string]any{"type": "success"}, finished.data["outcome"])
}

func TestTrailingToolMessagesAreTheNewTurn(t *testing.T) {
	in := RunAgentInput{Messages: []Message{
		{ID: "u1", Role: RoleUser, Content: "hi"},
		{ID: "a1", Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "x"}}, {ID: "c2", Function: ToolCallFunction{Name: "y"}}}},
		{ID: "t1", Role: RoleTool, ToolCallID: "c1", Content: "one"},
		{ID: "t2", Role: RoleTool, ToolCallID: "c2", Content: "two"},
	}}
	turn := in.NewTurnSDKMessages()
	require.Len(t, turn, 2)
	assert.Equal(t, "c1", turn[0].OfFunctionCallOutput.CallID)
	assert.Equal(t, "two", *turn[1].OfFunctionCallOutput.Output.OfString)

	// A user message after earlier tool results is an ordinary turn.
	in.Messages = append(in.Messages, Message{ID: "u2", Role: RoleUser, Content: "thanks"})
	turn = in.NewTurnSDKMessages()
	require.Len(t, turn, 1)
	require.NotNil(t, turn[0].OfInputMessage)
}

// postTurn posts a run request and returns the status, for turns that may be
// folded into a live run (204) rather than streamed.
func postTurn(t *testing.T, server *httptest.Server, agentName string, input RunAgentInput) int {
	t.Helper()
	body, err := json.Marshal(input)
	require.NoError(t, err)
	res, err := http.Post(server.URL+"/agents/"+agentName+"/run", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

var selectionCall = []Message{
	{ID: "u1", Role: RoleUser, Content: "summarize my selection"},
	{ID: "a1", Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Function: ToolCallFunction{Name: "get_selection", Arguments: "{}"}}}},
}

// In wait mode the client answers with the same tool message it would send to
// resume a pause; reaching a live run, it is kept for the waiting call.
func TestToolMessageAnswersAWaitingRun(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{
		{
			chunks:   []*responses.ResponseChunk{functionCallAdded("item-1", "call-1", "get_selection"), argsDelta("item-1", `{}`)},
			response: toolCallResponse("call-1", "get_selection", `{}`),
		},
		{
			chunks:   []*responses.ResponseChunk{messageAdded("msg_1"), textDelta("msg_1", "Revenue grew."), messageDone("msg_1")},
			response: assistantTextResponse("Revenue grew."),
		},
	}}
	broker := streambroker.NewMemoryStreamBroker()
	agent := agents.NewAgent(&agents.AgentOptions{
		Name: "Reader", StreamBroker: broker,
		ClientTools: agents.ClientToolOptions{Timeout: 5 * time.Second},
	}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"Reader": agent}))
	defer server.Close()

	first := make(chan []sseFrame, 1)
	go func() {
		first <- postRun(t, server, "Reader", RunAgentInput{ThreadID: "thread-wait", Tools: selectionInputTool, Messages: selectionCall[:1]})
	}()
	streamID := agents.StreamIDForThread("default", "thread-wait")
	require.Eventually(t, func() bool {
		active, _ := broker.IsActive(context.Background(), streamID)
		return active
	}, 2*time.Second, 10*time.Millisecond)

	status := postTurn(t, server, "Reader", RunAgentInput{
		ThreadID: "thread-wait", Tools: selectionInputTool,
		Messages: append(selectionCall, Message{ID: "t1", Role: RoleTool, ToolCallID: "call-1", Content: "Quarterly revenue grew 12%"}),
	})
	assert.Equal(t, http.StatusNoContent, status, "folded into the live run, not a run of its own")

	frames := <-first
	result, ok := findFrame(frames, "TOOL_CALL_RESULT")
	require.True(t, ok)
	assert.Equal(t, "Quarterly revenue grew 12%", result.data["content"])
	finished, ok := findFrame(frames, "RUN_FINISHED")
	require.True(t, ok)
	assert.Equal(t, map[string]any{"type": "success"}, finished.data["outcome"])
}

// A tool message for nothing the thread is waiting on — a result after its run
// moved on, or a duplicate — is acknowledged without starting a run.
func TestToolMessageForNothingPausedStartsNoRun(t *testing.T) {
	llm := &scriptedLLM{}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Reader"}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"Reader": agent}))
	defer server.Close()

	status := postTurn(t, server, "Reader", RunAgentInput{
		ThreadID: "thread-stale", Tools: selectionInputTool,
		Messages: append(selectionCall, Message{ID: "t1", Role: RoleTool, ToolCallID: "call-1", Content: "late"}),
	})
	assert.Equal(t, http.StatusNoContent, status)
	assert.Zero(t, llm.calls, "no run, so no model call")
}

// A provider that never sends output_item.done for a call must not leave it
// open: a client tool is run as soon as its call ends, and in wait mode the
// server is already waiting for that result.
func TestResponseCompletedEndsOpenToolCalls(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Translate(functionCallAdded("item-1", "call-1", "get_selection"))
	tr.Translate(argsDelta("item-1", "{}"))
	events := tr.Translate(&responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{}})
	require.NotEmpty(t, events)
	end, ok := events[0].(*ToolCallEndEvent)
	require.True(t, ok)
	assert.Equal(t, "call-1", end.ToolCallID)
	assert.Empty(t, tr.closeToolCalls(), "the call is ended only once")
}
