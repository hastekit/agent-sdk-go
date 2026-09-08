package agui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getFeed(t *testing.T, url string) FeedResponse {
	t.Helper()
	res, err := http.Get(url)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var out FeedResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&out))
	return out
}

// The case the feed exists for: the browser is in one conversation and
// another one starts. Nothing keyed to a thread could report that — least of
// all for a conversation that did not exist when the browser attached.
func TestRunFeedReportsAConversationTheClientIsNotIn(t *testing.T) {
	gate := newGateTool("gate")
	llm := &scriptedLLM{steps: []scriptedStep{
		{response: toolCallResponse("call_1", "gate", "{}")},
		{response: assistantTextResponse("done")},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "Helper",
		Tools: []agents.Tool{gate},
	}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	// Watching the namespace, from now, while sitting in no conversation.
	watched := make(chan FeedResponse, 1)
	go func() {
		watched <- getFeed(t, server.URL+"/agents/Helper/runs?wait=20s")
	}()

	// Something happens in conversation A.
	go postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "conversation-a",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "go"}},
	})

	var out FeedResponse
	select {
	case out = <-watched:
	case <-time.After(20 * time.Second):
		t.Fatal("the feed never reported the run")
	}

	require.NotEmpty(t, out.Events)
	assert.Equal(t, agents.RunEventStarted, out.Events[0].Event)
	assert.Equal(t, "conversation-a", out.Events[0].ThreadID, "the thread it happened in")
	assert.Equal(t, "default", out.Events[0].Namespace)
	assert.Equal(t, "Helper", out.Events[0].AgentName)
	assert.Equal(t, agents.StreamIDForThread("default", "conversation-a"), out.Events[0].StreamID,
		"so a client can attach without deriving anything")
	assert.NotEmpty(t, out.Cursor, "where to resume")

	close(gate.release)
}

// A run reports both ends, so a sidebar badge can go up and come down again.
func TestRunFeedReportsTheRunEnding(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{{response: assistantTextResponse("hi")}}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	// Take a cursor before anything happens, so nothing is missed.
	start := getFeed(t, server.URL+"/agents/Helper/runs?wait=0")

	postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "conversation-b",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "hi"}},
	})

	out := getFeed(t, server.URL+"/agents/Helper/runs?wait=5s&cursor="+start.Cursor)

	var events []string
	for _, event := range out.Events {
		events = append(events, event.Event)
	}
	assert.Equal(t, []string{agents.RunEventStarted, agents.RunEventFinished}, events)
}

// The cursor is what makes a run that started and ended while nobody was
// looking still reach the browser when it comes back.
func TestRunFeedReplaysWhatWasMissedFromTheCursor(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{
		{response: assistantTextResponse("one")},
		{response: assistantTextResponse("two")},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	start := getFeed(t, server.URL+"/agents/Helper/runs?wait=0")

	// Two conversations run to completion with nobody watching.
	for _, thread := range []string{"conversation-c", "conversation-d"} {
		postRun(t, server, "Helper", RunAgentInput{
			ThreadID: thread,
			Messages: []Message{{ID: "u1", Role: RoleUser, Content: "go"}},
		})
	}

	out := getFeed(t, server.URL+"/agents/Helper/runs?wait=0&cursor="+start.Cursor)

	threads := map[string]bool{}
	for _, event := range out.Events {
		threads[event.ThreadID] = true
	}
	assert.True(t, threads["conversation-c"], "a run nobody watched is still reported")
	assert.True(t, threads["conversation-d"])
}

// An empty cursor means "from now". A browser attaching for the first time
// wants what happens next, not a replay of the day.
func TestRunFeedWithoutACursorStartsFromNow(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{{response: assistantTextResponse("hi")}}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "conversation-old",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "hi"}},
	})

	out := getFeed(t, server.URL+"/agents/Helper/runs?wait=0")

	assert.Empty(t, out.Events, "what already happened is the thread list's job, not the feed's")
	assert.NotEmpty(t, out.Cursor)
}

// --- namespaces -------------------------------------------------------------

func TestFeedNamespaces(t *testing.T) {
	// Built through url.Values so a value with a space in it is encoded
	// rather than breaking the request line httptest parses.
	namespacesOf := func(namespaces string) []string {
		target := "/runs"
		if namespaces != "" {
			target += "?" + url.Values{"namespaces": {namespaces}}.Encode()
		}
		return feedNamespaces(httptest.NewRequest(http.MethodGet, target, nil), "default")
	}

	assert.Equal(t, []string{"default"}, namespacesOf(""), "the handler's own, for an ordinary UI")
	assert.Equal(t, []string{"a", "b"}, namespacesOf("a,b"))
	assert.Equal(t, []string{"a", "b"}, namespacesOf("a, b ,"), "trimmed, blanks dropped")
	assert.Equal(t, []string{"a"}, namespacesOf("a,a"), "deduped: one subscription each")
}

// A run in a namespace the client did not ask about is not its business.
func TestRunFeedIgnoresOtherNamespaces(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{{response: assistantTextResponse("hi")}}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	start := getFeed(t, server.URL+"/agents/Helper/runs?wait=0&namespaces=other")

	postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "conversation-e",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "hi"}},
	})

	out := getFeed(t, server.URL+"/agents/Helper/runs?wait=0&namespaces=other&cursor="+start.Cursor)
	assert.Empty(t, out.Events)
}

// The note the UI shows while a job runs is driven by these two events, on
// the run's own stream — so a task that starts while the user is watching is
// announced without waiting for a reload.
func TestRunEmitsBackgroundTaskStarted(t *testing.T) {
	tool := newBgTool()
	llm := &scriptedLLM{steps: []scriptedStep{
		{response: toolCallResponse("call_1", "index", "{}")},
		{response: assistantTextResponse("indexing started")},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "Helper",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	frames := postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-note",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "index the docs"}},
	})

	var announced int
	for _, frame := range frames {
		if frame.event != string(EventCustom) {
			continue
		}
		if frame.data["name"] != CustomNameBackgroundTaskStarted {
			continue
		}
		announced++

		value, ok := frame.data["value"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "task-1", value["taskId"])
		assert.Equal(t, "call_1", value["toolCallId"], "which call is still working")
		assert.Equal(t, "index", value["toolName"], "what the note names")
		assert.NotEmpty(t, value["streamId"], "where its progress goes")
	}

	assert.Equal(t, 1, announced, "announced on the run that started it, once")

	close(tool.release)
	agent.WaitForBackgroundTasks()
}
