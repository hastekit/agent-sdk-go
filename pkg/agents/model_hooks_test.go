package agents_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// budgetHook stands in for a check against a balance somebody keeps elsewhere.
type budgetHook struct {
	agents.NoopToolCallHook

	name    string
	log     *[]string
	before  func(call *agents.ModelCall) (agents.ModelCallHookResult, error)
	after   func(call *agents.ModelCall, result *agents.ModelCallResult) (agents.ModelCallHookResult, error)
	seen    []*agents.ModelCall
	charged []responses.Usage
}

func (h *budgetHook) GetName() string { return h.name }

func (h *budgetHook) BeforeModelCall(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
	*h.log = append(*h.log, "before:"+h.name)
	h.seen = append(h.seen, call)
	if h.before == nil {
		return agents.ContinueModelCall(), nil
	}
	return h.before(call)
}

func (h *budgetHook) AfterModelCall(ctx context.Context, call *agents.ModelCall, result *agents.ModelCallResult) (agents.ModelCallHookResult, error) {
	*h.log = append(*h.log, "after:"+h.name)
	h.charged = append(h.charged, result.Usage)
	if h.after == nil {
		return agents.ContinueModelCall(), nil
	}
	return h.after(call, result)
}

// The hooks run around every call to the model, once per loop iteration.
func TestModelCallHooks_RunAroundEveryCall(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	hook := &budgetHook{name: "budget", log: &log}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-model-hooks", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	// Two model calls — the one that asked for the tool, and the one that read
	// its result — so two rounds of hooks.
	assert.Equal(t, []string{
		"before:budget", "after:budget", "before:budget", "after:budget",
	}, log)
	assert.Equal(t, 2, llm.callCount())
}

// The before hook is told what the call is and who it belongs to, which is
// what a balance is looked up by.
func TestModelCallHooks_SeeTheCallAndItsRun(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{textResponse("done")}}
	hook := &budgetHook{name: "budget", log: &log}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace:  "test",
		ThreadID:   "thread-model-identity",
		Message:    userMessage("go"),
		RunContext: map[string]any{"tenant": "acme"},
	})

	require.Len(t, hook.seen, 1)
	call := hook.seen[0]
	assert.Equal(t, "main", call.AgentName)
	assert.Equal(t, "test", call.Namespace)
	assert.Equal(t, "thread-model-identity", call.ThreadID)
	assert.Equal(t, "acme", call.RunContext["tenant"])
	assert.Equal(t, 0, call.LoopIteration, "the run's own counter, which starts at zero")
}

// An exhausted budget answers for the model: the provider is never called, the
// user gets a message they can read, and the run completes rather than fails.
func TestModelCallHooks_HandledAnswersForTheModel(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{}}
	hook := &budgetHook{
		name: "budget",
		log:  &log,
		before: func(call *agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.HandleModelCall(agents.ModelCallText("You are out of credits.")), nil
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-out-of-credit", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 0, llm.callCount(), "the provider must not be called")
	assert.Contains(t, messagesText(out.Output), "You are out of credits.")
}

// A refusal that has nothing to say fails the run instead.
func TestModelCallHooks_ErrorFailsTheRun(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{}}
	hook := &budgetHook{
		name: "budget",
		log:  &log,
		before: func(call *agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.ContinueModelCall(), errors.New("billing unavailable")
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
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

// The after hook is told what the call reported using — the number a balance
// is drawn down by.
func TestModelCallHooks_AfterIsToldWhatTheCallUsed(t *testing.T) {
	var log []string

	reply := textResponse("done")
	reply.Usage = &responses.Usage{InputTokens: 120, OutputTokens: 34, TotalTokens: 154}

	llm := &scriptedLLM{script: []*responses.Response{reply}}
	hook := &budgetHook{name: "budget", log: &log}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-usage", Message: userMessage("go"),
	})

	require.Len(t, hook.charged, 1)
	assert.Equal(t, 154, hook.charged[0].TotalTokens)
	assert.Equal(t, 120, hook.charged[0].InputTokens)
}

// A hook that answers for the model still runs the after phase, so a hook that
// records spend sees the call it refused — at zero.
func TestModelCallHooks_AfterRunsEvenWhenHandled(t *testing.T) {
	var log []string

	llm := &scriptedLLM{script: []*responses.Response{}}
	hook := &budgetHook{
		name: "budget",
		log:  &log,
		before: func(call *agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.HandleModelCall(agents.ModelCallText("no credits")), nil
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-handled-after", Message: userMessage("go"),
	})

	assert.Equal(t, []string{"before:budget", "after:budget"}, log)
	require.Len(t, hook.charged, 1)
	assert.Equal(t, 0, hook.charged[0].TotalTokens, "a call never made costs nothing")
}

// The zero value carries on, and handling with no response is still handling.
func TestModelCallHookResult_ZeroValueContinues(t *testing.T) {
	assert.Equal(t, agents.ModelCallHookResult{}, agents.ContinueModelCall())
	assert.False(t, agents.ContinueModelCall().Handled)

	handled := agents.HandleModelCall(nil)
	assert.True(t, handled.Handled)
	assert.Nil(t, handled.Response)
}

// --- appending messages ----------------------------------------------------

// appendHook adds a note to each call, and records the state it was shown.
type appendHook struct {
	agents.NoopToolCallHook

	name string
	// before, when set, replaces the default "append one note" behaviour.
	before func(call *agents.ModelCall) (agents.ModelCallHookResult, error)
	after  func(call *agents.ModelCall) (agents.ModelCallHookResult, error)
	seen   []map[string]string
}

func (h *appendHook) GetName() string { return h.name }

func (h *appendHook) BeforeModelCall(_ context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
	// Snapshot: the runner merges each hook's state writes into call.State as
	// they are returned, so keeping the live map would record later writes too.
	h.seen = append(h.seen, maps.Clone(call.State))
	if h.before != nil {
		return h.before(call)
	}
	return agents.ContinueModelCall().WithMessages(responses.UserMessage("note from " + h.name)), nil
}

func (h *appendHook) AfterModelCall(_ context.Context, call *agents.ModelCall, _ *agents.ModelCallResult) (agents.ModelCallHookResult, error) {
	if h.after != nil {
		return h.after(call)
	}
	return agents.ContinueModelCall(), nil
}

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

func TestModelCallHooks_AppendedMessagesReachTheRequest(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("done")}}
	hook := &appendHook{name: "policy"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-append", Message: userMessage("go"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	texts := inputTexts(t, llm.request(0))
	require.NotEmpty(t, texts)
	assert.Equal(t, "note from policy", texts[len(texts)-1],
		"an appended note goes after the conversation, where the model reads it last")
}

// The note is for one call. It must not accumulate in the history the next
// call is built from — the same rule the loop's own budget reminder follows.
func TestModelCallHooks_AppendedMessagesAreNeverStored(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	hook := &appendHook{name: "policy"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Hooks: []agents.Hook{hook},
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

func TestModelCallHooks_AppendOrderFollowsHookOrder(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("done")}}
	first, second := &appendHook{name: "first"}, &appendHook{name: "second"}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{first, second},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-append-order", Message: userMessage("go"),
	})

	texts := inputTexts(t, llm.request(0))
	require.Len(t, texts, 3)
	assert.Equal(t, []string{"note from first", "note from second"}, texts[1:])
}

func TestModelCallHooks_AppendsDroppedWhenAHookAnswers(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{}}
	appender := &appendHook{name: "appender"}
	answerer := &appendHook{
		name: "answerer",
		before: func(*agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.HandleModelCall(agents.ModelCallText("answered without the model")), nil
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Hooks: []agents.Hook{appender, answerer},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-append-handled", Message: userMessage("go"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Zero(t, llm.callCount(), "there is no request left for the note to ride on")
}

func TestModelCallHooks_AfterCallAppendsAreIgnored(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	hook := &appendHook{
		name:   "late",
		before: func(*agents.ModelCall) (agents.ModelCallHookResult, error) { return agents.ContinueModelCall(), nil },
		after: func(*agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.ContinueModelCall().WithMessages(responses.UserMessage("too late")), nil
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-append-late", Message: userMessage("go"),
	})

	require.Equal(t, 2, llm.callCount())
	assert.Zero(t, countOf(inputTexts(t, llm.request(1)), "too late"),
		"by AfterModelCall the request is already gone")
}

// --- state -----------------------------------------------------------------

func TestModelCallHooks_StateUpdatesReachTheNextCall(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	hook := &appendHook{
		name: "counter",
		before: func(call *agents.ModelCall) (agents.ModelCallHookResult, error) {
			if call.State["warned"] == "1" {
				return agents.ContinueModelCall(), nil
			}
			return agents.ContinueModelCall().
				WithMessages(responses.UserMessage("first warning")).
				WithStateUpdates(map[string]string{"warned": "1"}), nil
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-state", Message: userMessage("go"),
	})

	require.Equal(t, 2, llm.callCount())
	assert.Equal(t, 1, countOf(inputTexts(t, llm.request(0)), "first warning"))
	assert.Zero(t, countOf(inputTexts(t, llm.request(1)), "first warning"),
		"a hook that remembers it has warned can warn once instead of every iteration")

	require.Len(t, hook.seen, 2)
	assert.Empty(t, hook.seen[0]["warned"])
	assert.Equal(t, "1", hook.seen[1]["warned"])
}

func TestModelCallHooks_StateUpdatesFromAfterCallApply(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	hook := &appendHook{
		name:   "recorder",
		before: func(*agents.ModelCall) (agents.ModelCallHookResult, error) { return agents.ContinueModelCall(), nil },
		after: func(*agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.ContinueModelCall().WithStateUpdates(map[string]string{"calls": "seen"}), nil
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-state-after", Message: userMessage("go"),
	})

	require.Len(t, hook.seen, 2)
	assert.Equal(t, "seen", hook.seen[1]["calls"], "recording spend after the call is the point of the after hook")
}

// Hooks and tools share one scratchpad, so what a tool wrote is what the next
// call's hooks read.
func TestModelCallHooks_SeeStateWrittenByTools(t *testing.T) {
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

	hook := &appendHook{
		name:   "reader",
		before: func(*agents.ModelCall) (agents.ModelCallHookResult, error) { return agents.ContinueModelCall(), nil },
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{tool},
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-state-shared", Message: userMessage("go"),
	})

	require.Len(t, hook.seen, 2)
	assert.Equal(t, "tool", hook.seen[1]["written_by"])
}

func TestModelCallHooks_LastWriterWinsOnAKey(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}
	write := func(value string) func(*agents.ModelCall) (agents.ModelCallHookResult, error) {
		return func(*agents.ModelCall) (agents.ModelCallHookResult, error) {
			return agents.ContinueModelCall().WithStateUpdates(map[string]string{"owner": value}), nil
		}
	}
	first := &appendHook{name: "first", before: write("first")}
	second := &appendHook{name: "second", before: write("second")}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Hooks: []agents.Hook{first, second},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-state-order", Message: userMessage("go"),
	})

	// Each hook snapshots the state it was handed, before writing its own.
	// So on the second call, the first hook sees what the pair left behind —
	// the later writer's value — and the second hook sees the first's write
	// from moments earlier, because a write lands as soon as it is returned.
	require.Len(t, first.seen, 2)
	require.Len(t, second.seen, 2)
	assert.Equal(t, "second", first.seen[1]["owner"], "the last writer of a key wins")
	assert.Equal(t, "first", second.seen[1]["owner"], "a later hook reads what an earlier one just wrote")
}

// The state a hook is shown is a copy. Writing to it must do nothing, so that
// the mistake fails the same way locally as it does inside a workflow, where
// the map was rebuilt from a serialized payload and cannot be written back.
func TestModelCallHooks_MutatingStateInPlaceDoesNothing(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "worker", "{}"),
		textResponse("done"),
	}}

	calls := 0
	hook := &appendHook{
		name: "mutator",
		before: func(call *agents.ModelCall) (agents.ModelCallHookResult, error) {
			calls++
			if calls == 1 && call.State != nil {
				call.State["sneaked"] = "in"
			}
			return agents.ContinueModelCall(), nil
		},
	}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "main",
		Tools: []agents.Tool{newFakeTool("worker", false, "tool ran")},
		Hooks: []agents.Hook{hook},
	}).WithLLM(llm)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-state-copy", Message: userMessage("go"),
	})

	require.Len(t, hook.seen, 2)
	assert.Empty(t, hook.seen[1]["sneaked"], "StateUpdates is the only way in")
}

// --- result builders -------------------------------------------------------

func TestModelCallHookResultBuilders(t *testing.T) {
	res := agents.ContinueModelCall().
		WithMessages(responses.UserMessage("a"), responses.UserMessage("b")).
		WithStateUpdates(map[string]string{"x": "1"}).
		WithStateUpdates(map[string]string{"y": "2", "x": "3"})

	assert.False(t, res.Handled)
	require.Len(t, res.AppendMessages, 2)
	assert.Equal(t, map[string]string{"x": "3", "y": "2"}, res.StateUpdates)
}

func TestModelCallHookResultBuildersComposeWithHandled(t *testing.T) {
	res := agents.HandleModelCall(agents.ModelCallText("done")).
		WithStateUpdates(map[string]string{"answered": "1"})

	assert.True(t, res.Handled)
	require.NotNil(t, res.Response)
	assert.Equal(t, map[string]string{"answered": "1"}, res.StateUpdates)
}

// Two results built from one base must not share a backing array.
func TestModelCallHookResultBuildersDoNotAlias(t *testing.T) {
	base := agents.ContinueModelCall().WithMessages(responses.UserMessage("base"))

	one := base.WithMessages(responses.UserMessage("one"))
	two := base.WithMessages(responses.UserMessage("two"))

	require.Len(t, base.AppendMessages, 1)
	require.Len(t, one.AppendMessages, 2)
	require.Len(t, two.AppendMessages, 2)
	assert.Equal(t, "one", one.AppendMessages[1].OfInputMessage.Content[0].OfInputText.Text)
	assert.Equal(t, "two", two.AppendMessages[1].OfInputMessage.Content[0].OfInputText.Text)
}
