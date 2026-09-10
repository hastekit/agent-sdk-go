package agui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The one knob both long polls share.
func TestWatchWait(t *testing.T) {
	waitOf := func(query string) time.Duration {
		r := httptest.NewRequest(http.MethodGet, "/watch?"+query, nil)
		return watchWait(r, defaultWatchWait)
	}

	assert.Equal(t, defaultWatchWait, waitOf(""), "unset falls back")
	assert.Equal(t, defaultWatchWait, waitOf("wait=soon"), "unreadable falls back")
	assert.Equal(t, 5*time.Second, waitOf("wait=5s"), "a duration")
	assert.Equal(t, 5*time.Second, waitOf("wait=5"), "bare seconds, which is what a browser sends")
	assert.Equal(t, maxWatchWait, waitOf("wait=99h"), "clamped: one client must not pin a connection open")
	assert.Zero(t, waitOf("wait=-1s"), "negative means do not wait")

	r := httptest.NewRequest(http.MethodGet, "/stream", nil)
	assert.Zero(t, watchWait(r, 0), "a rejoin answers at once unless asked to hold on")
}

// --- the whole flow ---------------------------------------------------------

// bgTool answers its call at once with a task id and finishes later, which is
// what makes a run start with no client asking for one.
type bgTool struct {
	*agents.BaseTool
	release chan struct{}
}

func newBgTool() *bgTool {
	return &bgTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "index",
			Description: utils.Ptr("indexes, slowly"),
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		}}},
		release: make(chan struct{}),
	}
}

func (t *bgTool) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID: params.ID, CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("started, job task-1")},
		},
		TaskID: "task-1",
	}, nil
}

func (t *bgTool) AwaitTask(_ context.Context, _ agents.BackgroundTaskRef, _ agents.ProgressReporter) (agents.BackgroundResult, error) {
	<-t.release
	return agents.BackgroundResult{Output: agents.BackgroundText("indexed 4210 documents")}, nil
}

// End to end, exactly as the browser does it: the turn finishes, the client
// falls back to watching, a background task completes and wakes the agent, the
// watch says so, and the client rejoins that run's stream.
//
// The woken run is gated on a tool so the rejoin has something to attach to.
// A real one takes as long as a model call, which is ample; without the gate
// this test's model answers instantly and the run is over before anyone can
// join — see the note on serveRunWatch about a run that begins and ends inside
// one gap.
func TestWatchThenRejoinPicksUpABackgroundTasksRun(t *testing.T) {
	tool := newBgTool()
	gate := newGateTool("gate")
	llm := &scriptedLLM{steps: []scriptedStep{
		{response: toolCallResponse("call_1", "index", "{}")},
		{response: assistantTextResponse("indexing started")},
		{response: toolCallResponse("call_2", "gate", "{}")},
		{
			chunks: []*responses.ResponseChunk{
				messageAdded("msg_1"),
				textDelta("msg_1", "the index is ready"),
				messageDone("msg_1"),
			},
			response: assistantTextResponse("the index is ready"),
		},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "Helper",
		Tools: []agents.Tool{tool, gate},
	}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	// The user's turn. It ends without waiting for the task.
	frames := postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-bg",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "index the docs"}},
	})
	require.Equal(t, EventRunFinished, EventType(frames[len(frames)-1].event))

	// Capture a cursor before releasing the task. An empty cursor starts at
	// request arrival, so starting the HTTP request in a goroutine is not a
	// subscription barrier: the background run may publish before it arrives.
	start := getFeed(t, server.URL+"/agents/Helper/runs?wait=0")
	require.NotEmpty(t, start.Cursor)

	close(tool.release)

	// Deliberately let the run start before polling. The saved cursor must
	// recover its event even when the browser was between requests. The gate
	// holds the run open for the subsequent stream rejoin.
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the background task did not start its follow-up run")
	}
	seen := getFeed(t, server.URL+"/agents/Helper/runs?wait=0&cursor="+start.Cursor)

	require.NotEmpty(t, seen.Events)
	assert.Equal(t, agents.RunEventStarted, seen.Events[0].Event)
	assert.Equal(t, "thread-bg", seen.Events[0].ThreadID)
	assert.Equal(t, agents.StreamIDForThread("default", "thread-bg"), seen.Events[0].StreamID)

	// And the client rejoins it, which is where the answer appears. The gate
	// is held until the rejoin has had time to attach, then released so the
	// run finishes and the stream closes under it.
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(gate.release)
	}()

	rejoined := getSSE(t, server.URL+"/agents/Helper/threads/thread-bg/stream")

	var events []string
	var text string
	for _, frame := range rejoined {
		events = append(events, frame.event)
		if frame.event == string(EventTextMessageContent) {
			if delta, ok := frame.data["delta"].(string); ok {
				text += delta
			}
		}
	}

	require.NotEmpty(t, events)
	assert.Equal(t, string(EventRunStarted), events[0], "the rejoin is a run of its own, opened properly")
	assert.Equal(t, string(EventRunFinished), events[len(events)-1])
	assert.Contains(t, text, "the index is ready", "the agent's reply to the task's result")

	// The task's result is announced on the run that took it in — and the
	// announcement is published before that run opens, so it only survives
	// because the reader holds back what precedes a run rather than dropping
	// it. It must still land after RUN_STARTED, which the AG-UI client
	// requires to come first.
	var announced int
	for i, frame := range rejoined {
		if frame.event != string(EventCustom) {
			continue
		}
		if frame.data["name"] != CustomNameBackgroundTaskCompleted {
			continue
		}
		announced++
		assert.Greater(t, i, 0, "never ahead of RUN_STARTED")

		value, ok := frame.data["value"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "task-1", value["taskId"])
		assert.Equal(t, "call_1", value["toolCallId"], "the call it belongs to")
	}
	assert.Equal(t, 1, announced, "announced once")

	agent.WaitForBackgroundTasks()
}
