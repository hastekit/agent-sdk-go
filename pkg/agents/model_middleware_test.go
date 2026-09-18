package agents_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// budgetMiddleware stands in for a check against a balance somebody keeps elsewhere.
// It records every call it was shown, the state it saw, and what each reply
// used; refuse answers in the model's place, fail fails the run, and note is
// put in front of the model for that call only.
type budgetMiddleware struct {
	agents.NoopMiddleware

	name   string
	log    *[]string
	refuse string
	fail   error
	note   string

	seen    []*agents.ModelCall
	states  []map[string]string
	charged []responses.Usage
}

func (h *budgetMiddleware) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
		if h.log != nil {
			*h.log = append(*h.log, "wrap:"+h.name)
		}
		h.seen = append(h.seen, call)
		h.states = append(h.states, maps.Clone(call.State))
		if h.fail != nil {
			return nil, h.fail
		}
		if h.refuse != "" {
			return agents.ModelCallText(h.refuse), nil
		}
		if h.note != "" {
			// On a copy: the loop keeps the request it handed in.
			noted := *request
			noted.Input.OfInputMessageList = append(slices.Clone(request.Input.OfInputMessageList), responses.UserMessage(h.note))
			request = &noted
		}
		resp, err := next(ctx, call, request)
		if err != nil {
			return nil, err
		}
		if resp.Usage != nil {
			h.charged = append(h.charged, *resp.Usage)
		}
		return resp, nil
	}
}

// The middlewares wrap every call to the model, once per loop iteration.
func TestModelCallMiddlewares_WrapEveryCall(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	middleware := &budgetMiddleware{name: "budget", log: &log}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Tools:       []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Middlewares: []agents.Middleware{middleware},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-model-middlewares", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	// Two model calls — the one that asked for the tool, and the one that read
	// its result — so two wraps.
	assert.Equal(t, []string{"wrap:budget", "wrap:budget"}, log)
	assert.Equal(t, 2, llm.callCount())
}

// The wrap is told what the call is and who it belongs to, which is what a
// balance is looked up by.
func TestModelCallMiddlewares_SeeTheCallAndItsRun(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("done")}}
	middleware := &budgetMiddleware{name: "budget"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Middlewares: []agents.Middleware{middleware},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace:  "test",
		ThreadID:   "thread-model-identity",
		Message:    userMessage("go"),
		RunContext: map[string]any{"tenant": "acme"},
	})

	require.Len(t, middleware.seen, 1)
	call := middleware.seen[0]
	assert.Equal(t, "main", call.AgentName)
	assert.Equal(t, "test", call.Namespace)
	assert.Equal(t, "thread-model-identity", call.ThreadID)
	assert.Equal(t, "acme", call.RunContext["tenant"])
	assert.Equal(t, 0, call.LoopIteration, "the run's own counter, which starts at zero")
}

// An exhausted budget answers for the model: the provider is never called, the
// user gets a message they can read, and the run completes rather than fails.
func TestModelCallMiddlewares_RefusalAnswersForTheModel(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{}}
	middleware := &budgetMiddleware{name: "budget", refuse: "You are out of credits."}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Middlewares: []agents.Middleware{middleware},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-out-of-credit", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 0, llm.callCount(), "the provider must not be called")
	assert.Contains(t, messagesText(out.Output), "You are out of credits.")
}

// A refusal that has nothing to say fails the run instead.
func TestModelCallMiddlewares_ErrorFailsTheRun(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{}}
	middleware := &budgetMiddleware{name: "budget", fail: errors.New("billing unavailable")}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Middlewares: []agents.Middleware{middleware},
	}).WithLLM(llm)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-billing-down", Message: userMessage("go"),
	})
	require.NoError(t, err)

	_, runErr := handle.Result()
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "billing unavailable")
	assert.Equal(t, 0, llm.callCount())
}

// Once next returns, the wrap holds the reply and what it reported using — the
// number a balance is drawn down by.
func TestModelCallMiddlewares_SeeWhatTheCallUsed(t *testing.T) {
	reply := textResponse("done")
	reply.Usage = &responses.Usage{InputTokens: 120, OutputTokens: 34, TotalTokens: 154}

	llm := &scriptedLLM{script: []*responses.Response{reply}}
	middleware := &budgetMiddleware{name: "budget"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Middlewares: []agents.Middleware{middleware},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-usage", Message: userMessage("go"),
	})

	require.Len(t, middleware.charged, 1)
	assert.Equal(t, 154, middleware.charged[0].TotalTokens)
	assert.Equal(t, 120, middleware.charged[0].InputTokens)
}

// --- notes for one call ------------------------------------------------------

// inputTexts is the plain text of every input message in a request, so a test
// can say what the model was actually shown.
func inputTexts(t *testing.T, req *responses.Request) []string {
	t.Helper()
	var out []string
	for _, msg := range req.Input.OfInputMessageList {
		switch {
		case msg.OfInputMessage != nil:
			for _, c := range msg.OfInputMessage.Content {
				if c.OfInputText != nil {
					out = append(out, c.OfInputText.Text)
				}
			}
		case msg.OfEasyInput != nil && msg.OfEasyInput.Content.OfString != nil:
			out = append(out, *msg.OfEasyInput.Content.OfString)
		}
	}
	return out
}

func countOf(texts []string, want string) int {
	n := 0
	for _, t := range texts {
		if t == want {
			n++
		}
	}
	return n
}

// A note handed to next on a copy of the request reaches the model, after the
// conversation, where it reads it last.
func TestModelCallMiddlewares_ANoteReachesTheRequest(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("done")}}
	middleware := &budgetMiddleware{name: "policy", note: "note from policy"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Middlewares: []agents.Middleware{middleware},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-append", Message: userMessage("go"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	texts := inputTexts(t, llm.request(0))
	require.NotEmpty(t, texts)
	assert.Equal(t, "note from policy", texts[len(texts)-1])
}

// The note is for one call. The loop keeps its own request, so it never enters
// the history the next call is built from — the same rule the loop's own
// budget reminder follows.
func TestModelCallMiddlewares_ANoteIsNeverStored(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	middleware := &budgetMiddleware{name: "policy", note: "note from policy"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Tools:       []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Middlewares: []agents.Middleware{middleware},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-append-once", Message: userMessage("go"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, 2, llm.callCount())

	second := inputTexts(t, llm.request(1))
	assert.Equal(t, 1, countOf(second, "note from policy"),
		"the second call carries this iteration's note only, not the first one's as well")
}

// Middlewares nest first-outermost, so the first middleware's note is appended first and
// the inner middleware's after it.
func TestModelCallMiddlewares_NoteOrderFollowsMiddlewareOrder(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("done")}}
	first, second := &budgetMiddleware{name: "first", note: "note from first"}, &budgetMiddleware{name: "second", note: "note from second"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Middlewares: []agents.Middleware{first, second},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-append-order", Message: userMessage("go"),
	})

	texts := inputTexts(t, llm.request(0))
	require.Len(t, texts, 3)
	assert.Equal(t, []string{"note from first", "note from second"}, texts[1:])
}

// A middleware inside one that answers for the model never runs, and neither does
// the provider: there is no request left for a note to ride on.
func TestModelCallMiddlewares_AnAnsweringMiddlewareSkipsWhatIsInsideIt(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{}}
	answerer := &budgetMiddleware{name: "answerer", refuse: "answered without the model"}
	inner := &budgetMiddleware{name: "inner", note: "never seen"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Middlewares: []agents.Middleware{answerer, inner},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-append-handled", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Zero(t, llm.callCount())
	assert.Empty(t, inner.seen)
}

// --- state -----------------------------------------------------------------

// A wrap reads the run's scratchpad as tools left it: what a tool wrote on one
// iteration is what the next call's wrap sees.
func TestModelCallMiddlewares_SeeStateWrittenByTools(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}

	tool := newFakeTool("worker", false, "tool ran")
	tool.execute = func(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return &agents.ToolCallResponse{
			FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
				ID:     params.ID,
				CallID: params.CallID,
				Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("tool ran")},
			},
			StateUpdates: map[string]string{"written_by": "tool"},
		}, nil
	}
	middleware := &budgetMiddleware{name: "reader"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:        "main",
		Tools:       []agents.Tool{tool},
		Middlewares: []agents.Middleware{middleware},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-state-shared", Message: userMessage("go"),
	})

	require.Len(t, middleware.states, 2)
	assert.Empty(t, middleware.states[0]["written_by"])
	assert.Equal(t, "tool", middleware.states[1]["written_by"])
}
