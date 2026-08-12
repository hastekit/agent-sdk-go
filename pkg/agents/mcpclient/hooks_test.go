package mcpclient

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolCallFor(name string, runContext map[string]any) *agents.ToolCall {
	return &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID:        "fc_1",
			CallID:    "call_1",
			Name:      name,
			Arguments: `{"q":"hello"}`,
		},
		RunContext: runContext,
	}
}

func outputOf(t *testing.T, resp *agents.ToolCallResponse) string {
	t.Helper()
	require.NotNil(t, resp)
	require.NotNil(t, resp.FunctionCallOutputMessage)
	require.NotNil(t, resp.Output.OfString)
	return *resp.Output.OfString
}

// A hook sees the call and the run context it arrived with — the pairing an
// access check needs.
func TestBeforeToolCall_SeesCallAndRunContext(t *testing.T) {
	var sawName, sawArgs string
	var sawContext map[string]any

	tool := NewLazyMcpTool(&mcp.Tool{Name: "search"}, "https://unreachable.test/mcp", "streamable-http",
		nil, nil, false, false, false,
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			sawName, sawArgs, sawContext = call.Name, call.Arguments, call.RunContext
			return nil, errors.New("stop here") // refuse, so the test never dials out
		})

	_, err := tool.Execute(t.Context(), toolCallFor("search", map[string]any{"jwt": "abc.def"}))
	require.NoError(t, err)

	assert.Equal(t, "search", sawName)
	assert.Equal(t, `{"q":"hello"}`, sawArgs)
	assert.Equal(t, "abc.def", sawContext["jwt"])
}

// A refusal is an answer, not a breakdown: the run continues and the model is
// told why, so it can say so or take another route.
func TestBeforeToolCall_RefusalReachesTheModel(t *testing.T) {
	tool := NewLazyMcpTool(&mcp.Tool{Name: "delete_user"}, "https://unreachable.test/mcp", "streamable-http",
		nil, nil, false, false, false,
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return nil, errors.New("not allowed for this user")
		})

	resp, err := tool.Execute(t.Context(), toolCallFor("delete_user", nil))
	require.NoError(t, err, "a refusal must not fail the run")
	assert.Contains(t, outputOf(t, resp), "not allowed for this user")
	assert.Equal(t, "call_1", resp.CallID, "the refusal still answers the call it refused")
}

// Hooks run in order and the first refusal stops the rest.
func TestBeforeToolCall_OrderAndShortCircuit(t *testing.T) {
	var ran []string

	tool := NewLazyMcpTool(&mcp.Tool{Name: "search"}, "https://unreachable.test/mcp", "streamable-http",
		nil, nil, false, false, false,
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			ran = append(ran, "first")
			return nil, nil
		},
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			ran = append(ran, "second")
			return nil, errors.New("denied")
		},
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			ran = append(ran, "third")
			return nil, nil
		})

	_, err := tool.Execute(t.Context(), toolCallFor("search", nil))
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, ran)
}

// A hook may amend the call rather than refuse it.
func TestBeforeToolCall_CanAmendTheCall(t *testing.T) {
	var seen string

	tool := NewLazyMcpTool(&mcp.Tool{Name: "search"}, "https://unreachable.test/mcp", "streamable-http",
		nil, nil, false, false, false,
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			call.Arguments = `{"q":"redacted"}`
			return nil, nil
		},
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			seen = call.Arguments
			return nil, errors.New("stop here")
		})

	_, err := tool.Execute(t.Context(), toolCallFor("search", nil))
	require.NoError(t, err)
	assert.Equal(t, `{"q":"redacted"}`, seen)
}

// Registering twice adds to the chain instead of replacing it, so two separate
// concerns can be wired without one dropping the other.
func TestWithBeforeToolCall_Appends(t *testing.T) {
	srv := &MCPClient{}
	noop := func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return nil, nil
	}

	WithBeforeToolCall(noop)(srv)
	WithBeforeToolCall(noop, noop)(srv)

	assert.Len(t, srv.beforeToolCall, 3)
}

// The tools ListTools hands out carry the hooks — this is the local runtime's
// path to Execute.
func TestBeforeToolCall_AttachedToListedTools(t *testing.T) {
	refused := errors.New("denied by policy")
	srv := &MCPClient{}
	WithBeforeToolCall(func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return nil, refused
	})(srv)

	tools := srv.buildLazyTools([]*mcp.Tool{{Name: "search"}}, nil, nil)
	require.Len(t, tools, 1)

	resp, err := tools[0].Execute(t.Context(), toolCallFor("search", nil))
	require.NoError(t, err)
	assert.Contains(t, outputOf(t, resp), "denied by policy")
}

// CallToolDirect is the path a durable runtime takes: the workflow holds only
// a serialized tool definition and calls back here to execute. The gate has to
// hold there too, or it would apply locally and silently not on Temporal.
func TestBeforeToolCall_AppliesOnCallToolDirect(t *testing.T) {
	var sawContext map[string]any
	srv := &MCPClient{Endpoint: "https://unreachable.test/mcp", Transport: "streamable-http"}
	WithBeforeToolCall(func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		sawContext = call.RunContext
		return nil, errors.New("denied on the durable path")
	})(srv)

	// Run context arrives as its own argument on this path; a hook should find
	// it on the call regardless.
	resp, err := srv.CallToolDirect(t.Context(), map[string]any{"jwt": "abc.def"}, toolCallFor("search", nil))
	require.NoError(t, err)
	assert.Contains(t, outputOf(t, resp), "denied on the durable path")
	assert.Equal(t, "abc.def", sawContext["jwt"])
}

// No hooks registered is the common case and must cost nothing.
func TestBeforeToolCall_NoneRegistered(t *testing.T) {
	resp, err := runBeforeToolCall(context.Background(), nil, toolCallFor("search", nil))
	assert.NoError(t, err)
	assert.Nil(t, resp, "nothing settled means the call goes to the server")

	// A nil entry is skipped rather than panicking the run.
	resp, err = runBeforeToolCall(context.Background(), []ToolCallHook{nil}, toolCallFor("search", nil))
	assert.NoError(t, err)
	assert.Nil(t, resp)
}

// A hook can answer the call itself: the server is never contacted and the
// model reads the hook's answer as the tool's result.
func TestBeforeToolCall_ShortCircuitsWithAResponse(t *testing.T) {
	tool := NewLazyMcpTool(&mcp.Tool{Name: "search"}, "https://unreachable.test/mcp", "streamable-http",
		nil, nil, false, false, false,
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return ToolCallResult(call, "answered from cache"), nil
		},
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			t.Error("a settled call must not reach later hooks")
			return nil, nil
		})

	resp, err := tool.Execute(t.Context(), toolCallFor("search", nil))
	require.NoError(t, err)
	assert.Equal(t, "answered from cache", outputOf(t, resp))
	assert.Equal(t, "call_1", resp.CallID)
	assert.Equal(t, "fc_1", resp.ID)
}

// A short-circuit works on the durable path too, not just locally.
func TestBeforeToolCall_ShortCircuitsOnCallToolDirect(t *testing.T) {
	srv := &MCPClient{Endpoint: "https://unreachable.test/mcp", Transport: "streamable-http"}
	WithBeforeToolCall(func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return ToolCallResult(call, "answered without the server"), nil
	})(srv)

	resp, err := srv.CallToolDirect(t.Context(), nil, toolCallFor("search", nil))
	require.NoError(t, err)
	assert.Equal(t, "answered without the server", outputOf(t, resp))
}

// A hook can pause the run instead of answering — how an unauthenticated
// caller gets sent somewhere to authenticate. A pause carries no result yet,
// so the response is passed through untouched.
func TestBeforeToolCall_CanPauseTheRun(t *testing.T) {
	call := toolCallFor("search", nil)
	interrupt := responses.Interrupt{
		FunctionCallMessage: *call.FunctionCallMessage,
		Mode:                responses.InterruptModeURL,
	}

	tool := NewLazyMcpTool(&mcp.Tool{Name: "search"}, "https://unreachable.test/mcp", "streamable-http",
		nil, nil, false, false, false,
		func(ctx context.Context, c *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return &agents.ToolCallResponse{Interrupts: []responses.Interrupt{interrupt}}, nil
		})

	resp, err := tool.Execute(t.Context(), call)
	require.NoError(t, err)
	require.Len(t, resp.Interrupts, 1)
	assert.Equal(t, responses.InterruptModeURL, resp.Interrupts[0].Mode)
	assert.Nil(t, resp.FunctionCallOutputMessage, "a pause has no result yet")
}

// A hand-built response can arrive without the ids that pair it with the
// function_call in history. Leaving it unpaired would break the next request
// to the provider rather than the hook that caused it.
func TestBeforeToolCall_StampsIdsOnAHandBuiltResponse(t *testing.T) {
	tool := NewLazyMcpTool(&mcp.Tool{Name: "search"}, "https://unreachable.test/mcp", "streamable-http",
		nil, nil, false, false, false,
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return &agents.ToolCallResponse{
				FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
					Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("hand built")},
				},
			}, nil
		})

	resp, err := tool.Execute(t.Context(), toolCallFor("search", nil))
	require.NoError(t, err)
	assert.Equal(t, "call_1", resp.CallID)
	assert.Equal(t, "fc_1", resp.ID)

	// And an empty response still answers the call rather than leaving it open.
	bare := NewLazyMcpTool(&mcp.Tool{Name: "search"}, "https://unreachable.test/mcp", "streamable-http",
		nil, nil, false, false, false,
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return &agents.ToolCallResponse{}, nil
		})

	resp, err = bare.Execute(t.Context(), toolCallFor("search", nil))
	require.NoError(t, err)
	assert.Equal(t, "", outputOf(t, resp))
	assert.Equal(t, "call_1", resp.CallID)
}
