package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testFormID = "details"

var testSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"full_name": map[string]any{"type": "string"},
	},
	"required": []string{"full_name"},
}

// elicitingServer stands up a tool that asks for a form before it can answer,
// counting how many times the handler ran so a test can prove the second pass
// happened.
func elicitingServer(t *testing.T, calls *int) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "eliciting", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "book",
		Description: "books something, asking the user for details first",
	}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		*calls++
		if answer, ok := req.Params.InputResponses[testFormID]; ok {
			result := answer.(*mcp.ElicitResult)
			if result.Action != "accept" {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "cancelled: " + result.Action}},
				}, nil, nil
			}
			name, _ := result.Content["full_name"].(string)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "booked for " + name}},
			}, nil, nil
		}
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{
				testFormID: &mcp.ElicitParams{
					Mode:            "form",
					Message:         "Who is this for?",
					RequestedSchema: testSchema,
				},
			},
			RequestState: "state-1",
		}, nil, nil
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { serverSession.Close() })

	// Connect through the package's own client, so the test exercises the
	// capabilities and handler the SDK actually ships.
	session, err := sdkClient.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })

	return session
}

func toolCall(callID string, resume []responses.InputMessageUnion) *agents.ToolCall {
	return &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID: "fc_" + callID, CallID: callID, Name: "book", Arguments: "{}",
		},
		ShouldResume:   len(resume) > 0,
		ResumeMessages: resume,
	}
}

func resumeWith(callID, action string, content string) []responses.InputMessageUnion {
	res := responses.InterruptResolution{CallID: callID, Action: action}
	if content != "" {
		res.Content = json.RawMessage(content)
	}
	return []responses.InputMessageUnion{{
		OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{
			Resolutions: []responses.InterruptResolution{res},
		},
	}}
}

func outputText(t *testing.T, resp *agents.ToolCallResponse) string {
	t.Helper()
	require.NotNil(t, resp.FunctionCallOutputMessage)
	require.NotNil(t, resp.FunctionCallOutputMessage.Output.OfString)
	return *resp.FunctionCallOutputMessage.Output.OfString
}

// The whole point of the bridge: a server asking for input pauses the run
// instead of failing the call with "client does not support elicitation", and
// the answer submitted against that pause completes the call.
func TestElicitation_PausesThenCompletesOnResume(t *testing.T) {
	var serverCalls int
	session := elicitingServer(t, &serverCalls)
	tool := NewMcpTool(&mcp.Tool{Name: "book"}, session, nil, false, false)

	// First pass: the tool pauses with the form the client must render.
	resp, err := tool.Execute(context.Background(), toolCall("call_1", nil))
	require.NoError(t, err)
	require.Len(t, resp.Interrupts, 1, "the server's input request must become an interrupt")
	assert.Nil(t, resp.FunctionCallOutputMessage, "a pause is not a tool result")

	intr := resp.Interrupts[0]
	assert.Equal(t, responses.InterruptModeForm, intr.Mode)
	assert.Equal(t, "call_1", intr.FunctionCallMessage.CallID)
	require.Len(t, intr.Elicitations, 1)
	assert.Equal(t, "Who is this for?", intr.Elicitations[0].Message)
	assert.NotNil(t, intr.Elicitations[0].RequestedSchema, "the client needs the schema to build a form")

	// Second pass: the user submitted the form. The agent loop merges a
	// pause's StateUpdates into thread state and hands them back, which is
	// how the resuming call knows which request it is answering.
	resume := toolCall("call_1", resumeWith("call_1", responses.InterruptActionApprove, `{"full_name":"Ada Lovelace"}`))
	resume.State = resp.StateUpdates
	resp, err = tool.Execute(context.Background(), resume)
	require.NoError(t, err)
	assert.Empty(t, resp.Interrupts, "the answered call must not pause again")
	assert.Equal(t, "booked for Ada Lovelace", outputText(t, resp))
}

// Declining reaches the server as MCP's "decline", so the tool decides what
// that means rather than the run failing.
func TestElicitation_RejectionReachesServerAsDecline(t *testing.T) {
	var serverCalls int
	session := elicitingServer(t, &serverCalls)
	tool := NewMcpTool(&mcp.Tool{Name: "book"}, session, nil, false, false)

	paused, err := tool.Execute(context.Background(), toolCall("call_1", nil))
	require.NoError(t, err)

	resume := toolCall("call_1", resumeWith("call_1", responses.InterruptActionReject, ""))
	resume.State = paused.StateUpdates
	resp, err := tool.Execute(context.Background(), resume)
	require.NoError(t, err)
	assert.Empty(t, resp.Interrupts)
	assert.Contains(t, outputText(t, resp), "cancelled: decline")
}

// The elicitation must not leak into an unrelated call: a fresh tool call with
// no answer pauses again rather than reusing the last one's reply.
func TestElicitation_AnswerDoesNotLeakAcrossCalls(t *testing.T) {
	var serverCalls int
	session := elicitingServer(t, &serverCalls)
	tool := NewMcpTool(&mcp.Tool{Name: "book"}, session, nil, false, false)

	paused, err := tool.Execute(context.Background(), toolCall("call_1", nil))
	require.NoError(t, err)
	resume := toolCall("call_1", resumeWith("call_1", responses.InterruptActionApprove, `{"full_name":"Ada"}`))
	resume.State = paused.StateUpdates
	_, err = tool.Execute(context.Background(), resume)
	require.NoError(t, err)

	// A different call carrying the answered call's state must still ask: the
	// record is keyed by call id, so nothing crosses over.
	fresh := toolCall("call_2", nil)
	fresh.State = paused.StateUpdates
	resp, err := tool.Execute(context.Background(), fresh)
	require.NoError(t, err)
	require.Len(t, resp.Interrupts, 1, "a new call with no answer must pause on its own")
}

// A tool that fails for an ordinary reason must still report a failure. The
// pause path is entered only for a recorded elicitation.
func TestElicitation_UnrelatedFailureIsNotAPause(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "boom", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "book"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, assertError{}
		})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	session, err := sdkClient.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	tool := NewMcpTool(&mcp.Tool{Name: "book"}, session, nil, false, false)
	resp, err := tool.Execute(ctx, toolCall("call_1", nil))
	require.NoError(t, err)
	assert.Empty(t, resp.Interrupts)
	assert.Contains(t, strings.ToLower(outputText(t, resp)), "boom")
}

type assertError struct{}

func (assertError) Error() string { return "boom" }

// The lazy path is what an agent actually runs: schemas are cached, and each
// call checks a connection out of the pool. This reproduces the reported
// failure — an agent calling an eliciting tool over streamable-http — so it
// covers the pool and the real transport, not just the in-memory one.
//
// The server is stateless because that is what negotiates protocol version
// 2026-07-28, where elicitation is fulfilled by the client in-process. See
// TestElicitation_StatefulServerIsRefusedClearly for the other case.
func TestElicitation_LazyToolOverHTTP(t *testing.T) {
	var serverCalls int
	hs := elicitingHTTPServer(t, &serverCalls, true)

	tool := NewLazyMcpTool(&mcp.Tool{Name: "book"}, hs, "streamable-http", nil, nil, true, false, false)

	resp, err := tool.Execute(context.Background(), toolCall("call_1", nil))
	require.NoError(t, err)
	require.Len(t, resp.Interrupts, 1, "the run must pause, not fail with 'client does not support elicitation'")
	assert.Equal(t, responses.InterruptModeForm, resp.Interrupts[0].Mode)

	resume := toolCall("call_1", resumeWith("call_1", responses.InterruptActionApprove, `{"full_name":"Ada Lovelace"}`))
	resume.State = resp.StateUpdates
	resp, err = tool.Execute(context.Background(), resume)
	require.NoError(t, err)
	assert.Empty(t, resp.Interrupts)
	// RequestState round-trips, so a server resumes where it left off instead
	// of asking again — and that takes one call, not two.
	assert.Equal(t, "booked for Ada Lovelace (state-1)", outputText(t, resp))
	assert.Equal(t, 2, serverCalls, "resume must answer the original question, not provoke a new one")
}

// A stateful HTTP server negotiates protocol version 2025-11-25, where the
// server sends elicitation/create itself rather than returning the request
// from the call. That is out of scope — no handler is registered for it — and
// what matters is that it fails legibly rather than hanging.
func TestElicitation_StatefulServerIsRefusedClearly(t *testing.T) {
	var serverCalls int
	hs := elicitingHTTPServer(t, &serverCalls, false)

	tool := NewLazyMcpTool(&mcp.Tool{Name: "book"}, hs, "streamable-http", nil, nil, true, false, false)
	resp, err := tool.Execute(context.Background(), toolCall("call_1", nil))
	require.NoError(t, err)
	assert.Empty(t, resp.Interrupts)
	assert.Contains(t, outputText(t, resp), "client does not support elicitation")
}

// elicitingHTTPServer starts the eliciting tool behind streamable-http and
// returns its URL, cleaning up the pooled connection before the server closes
// (the pool holds the session open, which would otherwise block shutdown).
func elicitingHTTPServer(t *testing.T, calls *int, stateless bool) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "eliciting", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "book", Description: "books something"},
		func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			*calls++
			if answer, ok := req.Params.InputResponses[testFormID]; ok {
				name, _ := answer.(*mcp.ElicitResult).Content["full_name"].(string)
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "booked for " + name + " (" + req.Params.RequestState + ")"}},
				}, nil, nil
			}
			return &mcp.CallToolResult{
				InputRequests: mcp.InputRequestMap{
					testFormID: &mcp.ElicitParams{
						Mode: "form", Message: "Who is this for?", RequestedSchema: testSchema,
					},
				},
				RequestState: "state-1",
			}, nil, nil
		})

	var opts *mcp.StreamableHTTPOptions
	if stateless {
		opts = &mcp.StreamableHTTPOptions{Stateless: true}
	}
	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, opts))

	t.Cleanup(hs.Close)
	t.Cleanup(func() { globalPool.Remove(hs.URL, "streamable-http", nil) })
	return hs.URL
}
