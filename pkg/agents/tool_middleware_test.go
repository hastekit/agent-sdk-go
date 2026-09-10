package agents_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tracingMiddleware records when it wrapped a call and when the call came back, so a
// test can assert on nesting and order. answer, when set, answers the call
// without running the tool; replace, when set, rewrites what came back.
type tracingMiddleware struct {
	agents.NoopMiddleware

	name    string
	log     *[]string
	answer  func(call *agents.ToolCall) *agents.ToolCallResponse
	replace func(call *agents.ToolCall, result *agents.ToolCallResponse) *agents.ToolCallResponse
}

func (h *tracingMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		*h.log = append(*h.log, "wrap:"+h.name)
		if h.answer != nil {
			return h.answer(call), nil
		}
		result, err := next(ctx, tool, call)
		if err != nil {
			return nil, err
		}
		*h.log = append(*h.log, "done:"+h.name)
		if h.replace != nil {
			return h.replace(call, result), nil
		}
		return result, nil
	}
}

// An agent's middlewares wrap every tool it calls, not just an MCP server's.
func TestAgentToolCallMiddlewares_WrapAPlainTool(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	tool := newFakeTool("worker", false, "tool ran")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Tools:       []agents.Tool{tool},
		Middlewares: []agents.Middleware{&tracingMiddleware{name: "audit", log: &log}},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-agent-middlewares", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, []string{"wrap:audit", "done:audit"}, log)
	assert.Equal(t, 1, tool.callCount())
}

// A middleware can refuse a call to any tool, not only an MCP one.
func TestAgentToolCallMiddlewares_CanRefuseAnyTool(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("understood"),
	}}
	tool := newFakeTool("worker", false, "tool ran")

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
		Middlewares: []agents.Middleware{&tracingMiddleware{
			name: "authz",
			log:  &log,
			answer: func(call *agents.ToolCall) *agents.ToolCallResponse {
				// A refusal is an answer, not a failure: the reason goes in the
				// response, where the model reads it and works around it. An
				// error here would end the run instead.
				return agents.ToolCallResult(call, "not allowed for this user")
			},
		}},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-agent-refuse", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 0, tool.callCount(), "a refused tool must not run")
	assert.Contains(t, messagesText(out.Output), "not allowed for this user")
}

// The agent hands its middlewares to the executor, which is what runs them. Nothing
// fails loudly if that injection is missed — the middlewares just never run — so
// assert the executor actually received them.
func TestAgentToolCallMiddlewares_InjectedIntoTheExecutor(t *testing.T) {
	middleware := &tracingMiddleware{name: "audit", log: new([]string)}
	executor := &agents.DefaultToolExecutor{}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:         "main",
		ToolExecutor: executor,
		Middlewares:  []agents.Middleware{middleware},
	})

	bound, ok := agent.ToolExecutor().(*agents.DefaultToolExecutor)
	require.True(t, ok)
	require.Len(t, bound.Middlewares, 1)
	assert.Same(t, middleware, bound.Middlewares[0])

	// Bound to a copy, so an executor shared between agents never runs another
	// agent's middleware.
	assert.Empty(t, executor.Middlewares)
}

// An executor built with middlewares of its own keeps them when the agent adds none.
func TestAgentToolCallMiddlewares_DoNotClearTheExecutorsOwn(t *testing.T) {
	middleware := &tracingMiddleware{name: "executor-own", log: new([]string)}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:         "main",
		ToolExecutor: &agents.DefaultToolExecutor{Middlewares: []agents.ToolCallMiddleware{middleware}},
	})

	bound, ok := agent.ToolExecutor().(*agents.DefaultToolExecutor)
	require.True(t, ok)
	require.Len(t, bound.Middlewares, 1)
	assert.Same(t, middleware, bound.Middlewares[0])
}

// runMiddlewares drives a set of middlewares around a tool that records whether it ran —
// the same thing an executor does, without one in the way.
func runMiddlewares(t *testing.T, middlewares []agents.ToolCallMiddleware, output string) (*agents.ToolCallResponse, bool) {
	t.Helper()

	toolRan := false
	call := &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{ID: "fc_1", CallID: "call_1", Name: "worker"},
	}

	resp, err := agents.ExecuteToolWithMiddleware(t.Context(), middlewares,
		agents.ExecutableToolCall{ToolName: "worker", Tool: newFakeTool("worker", false, output), ToolCall: call},
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			toolRan = true
			return agents.ToolCallResult(call, output), nil
		})
	require.NoError(t, err)
	return resp, toolRan
}

// A middleware that answers a call settles it: the tool is skipped, and so is every
// middleware nested inside it.
func TestToolCallMiddlewares_AnsweringSkipsTheToolAndInnerMiddlewares(t *testing.T) {
	var log []string

	resp, toolRan := runMiddlewares(t, []agents.ToolCallMiddleware{
		&tracingMiddleware{
			name: "cache",
			log:  &log,
			answer: func(call *agents.ToolCall) *agents.ToolCallResponse {
				return agents.ToolCallResult(call, "answered from cache")
			},
		},
		&tracingMiddleware{name: "inner", log: &log},
	}, "tool ran")

	assert.False(t, toolRan)
	assert.Equal(t, "answered from cache", *resp.Output.OfString)
	assert.Equal(t, []string{"wrap:cache"}, log, "a settled call never reaches the middlewares inside")
}

// An outer middleware sees what an inner one answered, so an audit middleware records
// refused calls too.
func TestToolCallMiddlewares_OuterMiddlewareSeesAnInnerRefusal(t *testing.T) {
	var log []string
	var seen string

	runMiddlewares(t, []agents.ToolCallMiddleware{
		&tracingMiddleware{
			name: "audit",
			log:  &log,
			replace: func(_ *agents.ToolCall, result *agents.ToolCallResponse) *agents.ToolCallResponse {
				seen = *result.Output.OfString
				return result
			},
		},
		&tracingMiddleware{
			name: "authz",
			log:  &log,
			answer: func(call *agents.ToolCall) *agents.ToolCallResponse {
				return agents.ToolCallResult(call, "denied")
			},
		},
	}, "tool ran")

	assert.Equal(t, "denied", seen)
	assert.Equal(t, []string{"wrap:audit", "wrap:authz", "done:audit"}, log)
}

// A middleware can replace what came back — redaction, or a post-filter that must
// not let something through. Withholding a result is still an answer; an error
// would end the run (see TestMiddlewareError_AfterTheToolStillEndsTheRun).
func TestToolCallMiddlewares_CanReplaceTheResult(t *testing.T) {
	var log []string

	resp, _ := runMiddlewares(t, []agents.ToolCallMiddleware{&tracingMiddleware{
		name: "redact",
		log:  &log,
		replace: func(call *agents.ToolCall, _ *agents.ToolCallResponse) *agents.ToolCallResponse {
			return agents.ToolCallResult(call, "[redacted]")
		},
	}}, "secret")
	assert.Equal(t, "[redacted]", *resp.Output.OfString)
}

// A hand-built response can arrive without the ids that pair it with the
// function_call in history. Leaving it unpaired would break the next request to
// the provider rather than the middleware that caused it.
func TestToolCallMiddlewares_StampIdsOnAHandBuiltResponse(t *testing.T) {
	var log []string

	resp, _ := runMiddlewares(t, []agents.ToolCallMiddleware{&tracingMiddleware{
		name: "settle",
		log:  &log,
		answer: func(*agents.ToolCall) *agents.ToolCallResponse {
			return &agents.ToolCallResponse{
				FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
					Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("hand built")},
				},
			}
		},
	}}, "tool ran")

	assert.Equal(t, "hand built", *resp.Output.OfString)
	assert.Equal(t, "call_1", resp.CallID)
	assert.Equal(t, "fc_1", resp.ID)
}

// recordingToolMiddleware keeps the tool it was shown.
type recordingToolMiddleware struct {
	agents.NoopMiddleware
	seen **agents.BaseTool
}

func (h *recordingToolMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		*h.seen = tool
		return next(ctx, tool, call)
	}
}

// What a middleware is shown is the tool itself, under the name the tool has rather
// than the one the model called it by — a prefixed MCP tool answers to
// "xyz__search" but is "search" to the server, and a policy is written against
// the latter. Its meta comes along for the same reason.
func TestToolCallMiddlewares_AreShownTheToolsOwnIdentity(t *testing.T) {
	var seen *agents.BaseTool

	tool := newFakeTool("xyz__search", false, "tool ran")
	tool.Name = "search"
	tool.Meta = map[string]any{"server_name": "search-server"}

	call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{
		ID: "fc_1", CallID: "call_1", Name: "xyz__search",
	}}

	_, err := agents.ExecuteToolWithMiddleware(t.Context(),
		[]agents.ToolCallMiddleware{&recordingToolMiddleware{seen: &seen}},
		agents.ExecutableToolCall{ToolName: "xyz__search", Tool: tool, ToolCall: call},
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return agents.ToolCallResult(call, "tool ran"), nil
		})
	require.NoError(t, err)

	require.NotNil(t, seen)
	assert.Equal(t, "search", seen.Name, "the tool's own name, not the model-facing one")
	assert.Equal(t, "xyz__search", seen.ToolUnion.OfFunction.Name, "and the model-facing one too")
	assert.Equal(t, "search-server", seen.Meta["server_name"])
}

// A tool that cannot describe itself still gets its call checked, under the name
// the call carries — and the stand-in has to be encodable, since under a durable
// runtime it crosses to the worker as an argument.
func TestToolCallMiddlewares_UndescribableToolStillReachesTheMiddleware(t *testing.T) {
	var seen *agents.BaseTool

	call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{
		ID: "fc_1", CallID: "call_1", Name: "worker",
	}}

	_, err := agents.ExecuteToolWithMiddleware(t.Context(),
		[]agents.ToolCallMiddleware{&recordingToolMiddleware{seen: &seen}},
		agents.ExecutableToolCall{ToolName: "worker", ToolCall: call},
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return agents.ToolCallResult(call, "tool ran"), nil
		})
	require.NoError(t, err, "a nil tool must not fail the call")

	require.NotNil(t, seen)
	assert.Equal(t, "worker", seen.Name)

	encoded, err := json.Marshal(seen)
	require.NoError(t, err, "the stand-in must survive a durable runtime's boundary")
	assert.Contains(t, string(encoded), "worker")
}

// A function tool has no name of its own to set — the model-facing name is its
// name — so the middleware gets that one filled in rather than an empty field. The
// tool's own BaseTool is left alone while that happens.
func TestToolCallMiddlewares_NameIsAlwaysFilledIn(t *testing.T) {
	var seen *agents.BaseTool

	tool := newFakeTool("worker", false, "tool ran")
	require.Empty(t, tool.Name, "a tool with no separate name of its own")

	call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{
		ID: "fc_1", CallID: "call_1", Name: "worker",
	}}

	_, err := agents.ExecuteToolWithMiddleware(t.Context(),
		[]agents.ToolCallMiddleware{&recordingToolMiddleware{seen: &seen}},
		agents.ExecutableToolCall{ToolName: "worker", Tool: tool, ToolCall: call},
		func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
			return agents.ToolCallResult(call, "tool ran"), nil
		})
	require.NoError(t, err)

	require.NotNil(t, seen)
	assert.Equal(t, "worker", seen.Name, "a middleware never has to fall back for itself")
	assert.Empty(t, tool.Name, "and the tool it was taken from is not written to")
}
