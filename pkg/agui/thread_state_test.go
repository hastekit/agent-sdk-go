package agui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type threadMessagesResponse struct {
	ThreadID string          `json:"threadId"`
	Messages []Message       `json:"messages"`
	Run      *ThreadRunState `json:"run"`
}

func getThreadMessages(t *testing.T, server *httptest.Server, agentName, threadID string) threadMessagesResponse {
	t.Helper()
	res, err := http.Get(server.URL + "/agents/" + agentName + "/threads/" + threadID + "/messages")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var out threadMessagesResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&out))
	return out
}

// The bug this exists for: a run pauses for approval, the browser reloads, and
// the card is gone — the agent is still waiting, but nothing on the page says
// so, because the card is drawn from an event only a live run emits.
func TestThreadMessagesReportsAPendingApproval(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{
		{response: toolCallResponse("call_1", "delete_user", `{"user_id":"123"}`)},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:    "Helper",
		Tools:   []agents.Tool{newApprovalTool("delete_user", "deleted")},
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	frames := postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-paused",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "delete user 123"}},
	})
	require.Equal(t, EventRunFinished, EventType(frames[len(frames)-1].event))

	// What a browser sees when it loads the thread fresh.
	loaded := getThreadMessages(t, server, "Helper", "thread-paused")

	require.NotNil(t, loaded.Run, "the reload has to be able to tell the run is waiting")
	assert.Equal(t, "paused", loaded.Run.Status)
	assert.True(t, loaded.Run.AwaitingApproval)
	require.Len(t, loaded.Run.Interrupts, 1)
	assert.Equal(t, "call_1", loaded.Run.Interrupts[0]["toolCallId"])
	assert.Equal(t, "delete_user", loaded.Run.Interrupts[0]["toolCallName"],
		"the same shape the live on_interrupt event carries, so one card draws both")
	require.Len(t, loaded.Run.PendingToolCalls, 1)
}

// A thread whose last run finished has nothing outstanding, and says so by
// omission — a client can treat run's presence as "there is something to act
// on" rather than having to inspect it.
func TestThreadMessagesReportsNothingForASettledThread(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{{response: assistantTextResponse("hi")}}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:    "Helper",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-settled",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "hi"}},
	})

	loaded := getThreadMessages(t, server, "Helper", "thread-settled")
	assert.Nil(t, loaded.Run)
}

// A task still working is reported too, so a reload can say so rather than
// looking like the agent simply stopped talking.
func TestThreadMessagesReportsRunningBackgroundTasks(t *testing.T) {
	tool := newBgTool()
	llm := &scriptedLLM{steps: []scriptedStep{
		{response: toolCallResponse("call_1", "index", "{}")},
		{response: assistantTextResponse("indexing started")},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:    "Helper",
		Tools:   []agents.Tool{tool},
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-working",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "index the docs"}},
	})

	loaded := getThreadMessages(t, server, "Helper", "thread-working")

	require.NotNil(t, loaded.Run)
	require.Len(t, loaded.Run.BackgroundTasks, 1)
	task := loaded.Run.BackgroundTasks[0]
	assert.Equal(t, "task-1", task.TaskID)
	assert.Equal(t, "call_1", task.CallID, "which call it belongs to")
	assert.Equal(t, "index", task.ToolName)
	assert.Equal(t, agents.StreamIDForTask("default", "thread-working", "task-1"), task.StreamID,
		"so a reload can watch its progress without having seen the run that started it")
	assert.NotEmpty(t, task.StartedAt)

	close(tool.release)
	agent.WaitForBackgroundTasks()
}

func TestThreadRunStateOfAnEmptyTranscript(t *testing.T) {
	assert.Nil(t, threadRunState(nil))
	assert.Nil(t, threadRunState([]history.ConversationMessage{{}}), "no run meta, nothing to say")
}
