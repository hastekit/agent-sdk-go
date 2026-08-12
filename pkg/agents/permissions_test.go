package agents_test

import (
	"strings"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// annotatedTool is a tool that declares what it does, the way an MCP server
// annotates one.
func annotatedTool(name, output string, annotations *agents.ToolAnnotations) *fakeTool {
	t := newFakeTool(name, false, output)
	t.BaseTool.Annotations = annotations
	return t
}

func destructiveTool(name, output string) *fakeTool {
	return annotatedTool(name, output, &agents.ToolAnnotations{DestructiveHint: utils.Ptr(true)})
}

// Default mode gates a tool that says it is destructive, even though nobody
// configured it as requiring approval.
func TestPermissionMode_DefaultAsksBeforeDestructiveTool(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_wipe", "wipe_disk", "{}"),
	}}
	wipe := destructiveTool("wipe_disk", "wiped")
	agent := newScriptedAgent("main", llm, nil, nil, []agents.Tool{wipe}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-destructive",
		Message:   userMessage("wipe it"),
	})

	requireStatus(t, out, agentstate.RunStatusPaused)
	requireSinglePendingApproval(t, out, "wipe_disk", "call_wipe")
	assert.Equal(t, 0, wipe.callCount(), "a destructive tool must not run before approval")
}

// A tool that declares nothing keeps running unattended. Annotations are
// recent; every tool written before them would otherwise start prompting.
func TestPermissionMode_DefaultRunsUnannotatedTool(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_read", "read_file", "{}"),
		textResponse("read it"),
	}}
	read := newFakeTool("read_file", false, "file contents")
	agent := newScriptedAgent("main", llm, nil, nil, []agents.Tool{read}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-unannotated",
		Message:   userMessage("read it"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 1, read.callCount())
}

// A tool that declares itself read-only is never gated, whatever else it says.
func TestPermissionMode_DefaultRunsReadOnlyTool(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_search", "search", "{}"),
		textResponse("searched"),
	}}
	search := annotatedTool("search", "results", &agents.ToolAnnotations{
		ReadOnlyHint:    utils.Ptr(true),
		DestructiveHint: utils.Ptr(true), // meaningless on a read-only tool
	})
	agent := newScriptedAgent("main", llm, nil, nil, []agents.Tool{search}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-readonly",
		Message:   userMessage("search"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 1, search.callCount())
}

// allow_all runs an unattended turn end to end: neither the tool's configured
// approval gate nor its destructive hint pauses it.
func TestPermissionMode_AllowAllRunsEverything(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_danger", "dangerous", "{}"),
		textResponse("done"),
	}}
	danger := newFakeTool("dangerous", true, "dangerous done")
	danger.BaseTool.Annotations = &agents.ToolAnnotations{DestructiveHint: utils.Ptr(true)}
	agent := newScriptedAgent("main", llm, nil, nil, []agents.Tool{danger}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace:      "test",
		ThreadID:       "thread-allow-all",
		Message:        userMessage("do it, nobody is watching"),
		PermissionMode: agents.PermissionModeAllowAll,
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 1, danger.callCount())
	assert.Empty(t, out.Interrupts)
}

// threadWithPermissions opens a thread with one ordinary turn — a standing
// decision attaches to the conversation, so there has to be one — and then
// records the decision the test is about.
func threadWithPermissions(t *testing.T, threadID string, tools []agents.Tool, llm *scriptedLLM, decide func(*history.ToolPermissions)) *agents.Agent {
	t.Helper()

	hist := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	agent := newScriptedAgent("main", llm, hist, nil, tools, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  threadID,
		Message:   userMessage("hello"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	require.NoError(t, hist.UpdateThreadToolPermissions(t.Context(), "test", threadID, decide))
	return agent
}

// A denied tool is refused outright: the run doesn't pause, the tool doesn't
// run, and the model is told so it can find another way.
func TestPermissions_ThreadDenyRefusesWithoutAsking(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		textResponse("hi"),
		toolCallResponse("call_send", "send_email", "{}"),
		textResponse("I can't send email, so here is a draft instead"),
	}}
	send := newFakeTool("send_email", false, "sent")
	agent := threadWithPermissions(t, "thread-deny", []agents.Tool{send}, llm, func(p *history.ToolPermissions) {
		p.Deny("send_email")
	})

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-deny",
		Message:   userMessage("email everyone"),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 0, send.callCount(), "a denied tool must never execute")
	assert.Empty(t, out.Interrupts, "a denial is not a question for the user")
	assert.Contains(t, messagesText(out.Output), "denied")
}

// The thread's standing decisions outrank the turn's mode — allow-all is the
// caller saying "don't ask me", not "ignore what I already decided".
func TestPermissions_ThreadDenyOutranksAllowAll(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		textResponse("hi"),
		toolCallResponse("call_send", "send_email", "{}"),
		textResponse("could not send"),
	}}
	send := newFakeTool("send_email", false, "sent")
	agent := threadWithPermissions(t, "thread-deny-allow-all", []agents.Tool{send}, llm, func(p *history.ToolPermissions) {
		p.Deny("send_email")
	})

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace:      "test",
		ThreadID:       "thread-deny-allow-all",
		Message:        userMessage("email everyone"),
		PermissionMode: agents.PermissionModeAllowAll,
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 0, send.callCount())
}

// "Don't ask me again" holds for every later turn: the decision is in the
// thread's meta, so no turn has to carry it.
func TestPermissions_ThreadAllowPersistsAcrossTurns(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		textResponse("hi"),
		toolCallResponse("call_danger_1", "dangerous", "{}"),
		textResponse("first done"),
		toolCallResponse("call_danger_2", "dangerous", "{}"),
		textResponse("second done"),
	}}
	danger := newFakeTool("dangerous", true, "dangerous done")
	agent := threadWithPermissions(t, "thread-always-allow", []agents.Tool{danger}, llm, func(p *history.ToolPermissions) {
		p.Allow("dangerous")
	})

	for turn, prompt := range []string{"do the dangerous thing", "again"} {
		out := runAgent(t, agent, &agents.AgentInput{
			Namespace: "test",
			ThreadID:  "thread-always-allow",
			Message:   userMessage(prompt),
		})
		requireStatus(t, out, agentstate.RunStatusCompleted)
		assert.Empty(t, out.Interrupts, "turn %d should not have asked", turn+1)
	}

	assert.Equal(t, 2, danger.callCount(), "the standing decision should outlive the turn that recorded it")
}

// A standing decision can be reversed mid-conversation.
func TestPermissions_ThreadDecisionCanBeReversed(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		textResponse("hi"),
		toolCallResponse("call_1", "risky", "{}"),
		textResponse("first done"),
		toolCallResponse("call_2", "risky", "{}"),
		textResponse("refused the second time"),
	}}
	risky := newFakeTool("risky", true, "risky done")
	hist := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	agent := newScriptedAgent("main", llm, hist, nil, []agents.Tool{risky}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-reversal", Message: userMessage("hello"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	require.NoError(t, hist.UpdateThreadToolPermissions(t.Context(), "test", "thread-reversal", func(p *history.ToolPermissions) {
		p.Allow("risky")
	}))
	out = runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-reversal", Message: userMessage("go ahead"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, 1, risky.callCount())

	require.NoError(t, hist.UpdateThreadToolPermissions(t.Context(), "test", "thread-reversal", func(p *history.ToolPermissions) {
		p.Deny("risky")
	}))
	out = runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-reversal", Message: userMessage("actually, never do that again"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 1, risky.callCount(), "the allow should have been revoked, not merely outvoted")
	assert.Contains(t, messagesText(out.Output), "denied")
}

// rememberMessage answers a paused call and asks for the answer to stand for
// the whole thread — the "don't ask me again" checkbox.
func rememberMessage(callID string, approve bool) history.Message {
	action := responses.InterruptActionReject
	if approve {
		action = responses.InterruptActionApprove
	}
	return messages.New("user", []responses.InputMessageUnion{{
		OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{
			Resolutions: []responses.InterruptResolution{
				{CallID: callID, Action: action, RememberAction: true},
			},
		},
	}})
}

// "Approve, and don't ask again": the call runs now, and the next turn's call
// to the same tool doesn't pause.
func TestRememberAction_ApproveBecomesStandingAllow(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "dangerous", "{}"),
		textResponse("first done"),
		toolCallResponse("call_2", "dangerous", "{}"),
		textResponse("second done"),
	}}
	danger := newFakeTool("dangerous", true, "dangerous done")
	hist := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	agent := newScriptedAgent("main", llm, hist, nil, []agents.Tool{danger}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-remember-allow", Message: userMessage("do it"),
	})
	requireStatus(t, out, agentstate.RunStatusPaused)

	out = runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-remember-allow", Message: rememberMessage("call_1", true),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, 1, danger.callCount())

	permissions, err := hist.ThreadToolPermissions(t.Context(), "test", "thread-remember-allow")
	require.NoError(t, err)
	assert.Equal(t, []string{"dangerous"}, permissions.AlwaysAllow)

	// The next turn asks nothing.
	out = runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-remember-allow", Message: userMessage("again"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Empty(t, out.Interrupts)
	assert.Equal(t, 2, danger.callCount())
}

// "Reject, and never again": the call is declined now, and the next turn's
// call to the same tool is refused outright rather than asked about.
func TestRememberAction_RejectBecomesStandingDeny(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "dangerous", "{}"),
		textResponse("understood"),
		toolCallResponse("call_2", "dangerous", "{}"),
		textResponse("still not allowed"),
	}}
	danger := newFakeTool("dangerous", true, "dangerous done")
	hist := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	agent := newScriptedAgent("main", llm, hist, nil, []agents.Tool{danger}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-remember-deny", Message: userMessage("do it"),
	})
	requireStatus(t, out, agentstate.RunStatusPaused)

	out = runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-remember-deny", Message: rememberMessage("call_1", false),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, 0, danger.callCount())

	permissions, err := hist.ThreadToolPermissions(t.Context(), "test", "thread-remember-deny")
	require.NoError(t, err)
	assert.Equal(t, []string{"dangerous"}, permissions.AlwaysDeny)

	out = runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-remember-deny", Message: userMessage("try again"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Empty(t, out.Interrupts, "the thread already refused this tool")
	assert.Equal(t, 0, danger.callCount())
	assert.Contains(t, messagesText(out.Output), "denied")
}

// A plain answer stays a one-off: the same tool is asked about again.
func TestRememberAction_UnsetLeavesTheAnswerOneOff(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "dangerous", "{}"),
		textResponse("first done"),
		toolCallResponse("call_2", "dangerous", "{}"),
	}}
	danger := newFakeTool("dangerous", true, "dangerous done")
	hist := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	agent := newScriptedAgent("main", llm, hist, nil, []agents.Tool{danger}, nil)

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-one-off", Message: userMessage("do it"),
	})
	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-one-off", Message: approvalMessage([]string{"call_1"}, nil),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	permissions, err := hist.ThreadToolPermissions(t.Context(), "test", "thread-one-off")
	require.NoError(t, err)
	assert.True(t, permissions.IsEmpty())

	out = runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-one-off", Message: userMessage("again"),
	})
	requireStatus(t, out, agentstate.RunStatusPaused)
	requireSinglePendingApproval(t, out, "dangerous", "call_2")
}

// Changing your mind moves the tool between the lists rather than leaving it
// on both — where the deny would win forever.
func TestRememberAction_ReversalKeepsTheListsExclusive(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "dangerous", "{}"),
		textResponse("first done"),
		toolCallResponse("call_2", "dangerous", "{}"),
		textResponse("second done"),
	}}
	danger := newFakeTool("dangerous", true, "dangerous done")
	hist := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	agent := newScriptedAgent("main", llm, hist, nil, []agents.Tool{danger}, nil)

	// Deny it for good...
	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-flip", Message: userMessage("do it"),
	})
	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-flip", Message: rememberMessage("call_1", false),
	})

	// ...then allow it for good, out of band.
	require.NoError(t, hist.UpdateThreadToolPermissions(t.Context(), "test", "thread-flip", func(p *history.ToolPermissions) {
		p.Allow("dangerous")
	}))

	permissions, err := hist.ThreadToolPermissions(t.Context(), "test", "thread-flip")
	require.NoError(t, err)
	assert.Equal(t, []string{"dangerous"}, permissions.AlwaysAllow)
	assert.Empty(t, permissions.AlwaysDeny, "a tool must never stand on both lists")

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-flip", Message: userMessage("now do it"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Equal(t, 1, danger.callCount())
}

// The decision table, without a run around it.
func TestPermissionPolicy_Decide(t *testing.T) {
	needsApproval := newFakeTool("gated", true, "")
	destructive := destructiveTool("destructive", "")
	plain := newFakeTool("plain", false, "")

	cases := []struct {
		name   string
		policy agents.PermissionPolicy
		tool   agents.Tool
		want   agents.ToolPermission
	}{
		{"default gates a tool configured for approval", agents.PermissionPolicy{}, needsApproval, agents.ToolPermissionAsk},
		{"default gates a declared-destructive tool", agents.PermissionPolicy{}, destructive, agents.ToolPermissionAsk},
		{"default allows anything else", agents.PermissionPolicy{}, plain, agents.ToolPermissionAllow},
		{
			"allow-all overrides both gates",
			agents.PermissionPolicy{Mode: agents.PermissionModeAllowAll},
			destructive, agents.ToolPermissionAllow,
		},
		{
			"an allowed name skips the gate",
			agents.PermissionPolicy{AlwaysAllow: []string{"gated"}},
			needsApproval, agents.ToolPermissionAllow,
		},
		{
			"a denied name beats an allowed one",
			agents.PermissionPolicy{AlwaysAllow: []string{"plain"}, AlwaysDeny: []string{"plain"}},
			plain, agents.ToolPermissionDeny,
		},
		{
			"a denied name that resolves to no tool is still denied",
			agents.PermissionPolicy{AlwaysDeny: []string{"ghost"}},
			nil, agents.ToolPermissionDeny,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "ghost"
			if tc.tool != nil {
				name = tc.tool.Tool(t.Context()).OfFunction.Name
			}
			assert.Equal(t, tc.want, tc.policy.Decide(name, tc.tool))
		})
	}
}

// The refusal handed to the model has to read as final, or it retries the same
// call until the loop budget runs out.
func TestPermissions_DenialTellsModelNotToRetry(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		textResponse("hi"),
		toolCallResponse("call_send", "send_email", "{}"),
		textResponse("acknowledged"),
	}}
	send := newFakeTool("send_email", false, "sent")
	agent := threadWithPermissions(t, "thread-deny-text", []agents.Tool{send}, llm, func(p *history.ToolPermissions) {
		p.Deny("send_email")
	})

	runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-deny-text",
		Message:   userMessage("email everyone"),
	})

	// The last request carries the refusal the model has to act on.
	require.Equal(t, 3, llm.callCount())
	sent := messagesText(llm.request(2).Input.OfInputMessageList)
	assert.True(t, strings.Contains(sent, "not permitted"), "refusal should state the tool is not permitted: %s", sent)
	assert.Contains(t, sent, "Do not try it again")
}
