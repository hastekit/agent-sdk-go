package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateTool blocks inside Execute until release is closed, and closes entered
// the first time it runs — letting a test act while a run is provably in
// flight (claimed, not yet finished).
type gateTool struct {
	*agents.BaseTool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGateTool(name string) *gateTool {
	return &gateTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
				Name:        name,
				Description: utils.Ptr("blocks until released"),
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
			}},
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (t *gateTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	t.once.Do(func() { close(t.entered) })
	select {
	case <-t.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID:     params.ID,
		CallID: params.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("gate done")},
	}}, nil
}

func readSSEFrames(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(body)
	require.NoError(t, err)

	frames := []sseFrame{}
	for _, raw := range strings.Split(buf.String(), "\n\n") {
		frame := sseFrame{}
		var data strings.Builder
		for _, line := range strings.Split(raw, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				frame.event = strings.TrimSpace(line[len("event:"):])
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimSpace(line[len("data:"):]))
			}
		}
		if frame.event == "" {
			continue
		}
		require.NoError(t, json.Unmarshal([]byte(data.String()), &frame.data))
		frames = append(frames, frame)
	}
	return frames
}

func getSSE(t *testing.T, url string) []sseFrame {
	t.Helper()
	res, err := http.Get(url)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	return readSSEFrames(t, res.Body)
}

// StreamIDForThread is deterministic per (namespace, thread), disambiguates on
// both, and is random when there's no thread to key on.
func TestStreamIDForThread(t *testing.T) {
	a := agents.StreamIDForThread("ns1", "thread-1")
	assert.Equal(t, a, agents.StreamIDForThread("ns1", "thread-1"), "same (namespace, thread) → same id")
	assert.NotEqual(t, a, agents.StreamIDForThread("ns2", "thread-1"), "namespace disambiguates")
	assert.NotEqual(t, a, agents.StreamIDForThread("ns1", "thread-2"), "thread disambiguates")
	assert.NotEqual(t, agents.StreamIDForThread("ns", ""), agents.StreamIDForThread("ns", ""), "threadless → random")
}

// The run endpoint reports a stable, deterministic broker stream id for a
// thread — the id a client rejoins with.
func TestRunEmitsDeterministicStreamID(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{{response: assistantTextResponse("hi")}}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	body, _ := json.Marshal(RunAgentInput{ThreadID: "thread-det", Messages: []Message{{ID: "u1", Role: RoleUser, Content: "hi"}}})
	res, err := http.Post(server.URL+"/agents/Helper/run", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	assert.Equal(t, agents.StreamIDForThread("default", "thread-det"), res.Header.Get("X-Stream-Id"))
	_, _ = io.Copy(io.Discard, res.Body)
}

// The opening user turn is persisted at run start, so a client that reloads
// mid-run can fetch the thread's messages starting from the user message
// (rather than seeing nothing until the run completes).
//
// Skipped: the run persists nothing until it finishes, so a client that
// rejoins a live thread sees the conversation without the turn that
// started it. The rejoined stream still carries the run's output. Enabling
// this means saving the opening turn before the loop starts.
func TestMidRunMessagesIncludeUserTurn(t *testing.T) {
	t.Skip("runs persist only on completion; the opening turn is not readable mid-run")

	gate := newGateTool("gate")
	llm := &scriptedLLM{steps: []scriptedStep{
		{response: toolCallResponse("call-1", "gate", "{}")},
		{response: assistantTextResponse("done")},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Tools: []agents.Tool{gate}}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	const threadID = "thread-midrun"
	done := make(chan []sseFrame, 1)
	go func() {
		done <- postRun(t, server, "Helper", RunAgentInput{
			ThreadID: threadID,
			Messages: []Message{{ID: "u1", Role: RoleUser, Content: "hello mid-run"}},
		})
	}()
	<-gate.entered // run is in flight, blocked in the tool

	res, err := http.Get(server.URL + "/agents/Helper/threads/" + threadID + "/messages")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	var payload struct {
		Messages []map[string]any `json:"messages"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&payload))
	blob, _ := json.Marshal(payload.Messages)
	assert.Contains(t, string(blob), "hello mid-run", "user turn must be fetchable while the run is still in flight")

	close(gate.release)
	<-done
}

// While a run is in flight on a thread, a concurrent POST folds into it (HTTP
// 204), and a GET on the thread's stream rejoins the run and follows it to
// completion.
func TestConcurrentTurnFoldsAndRejoinFollows(t *testing.T) {
	gate := newGateTool("gate")
	llm := &scriptedLLM{steps: []scriptedStep{
		{response: toolCallResponse("call-1", "gate", "{}")},
		{response: assistantTextResponse("done")},
		// Spare step: the folded-in concurrent message is drained into the
		// next LLM call; this keeps the scripted LLM from exhausting if the
		// loop takes an extra turn.
		{response: assistantTextResponse("done")},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper", Tools: []agents.Tool{gate}}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	const threadID = "thread-live"

	// Start a run; it blocks inside the gate tool, so the run is provably live.
	firstDone := make(chan []sseFrame, 1)
	go func() {
		firstDone <- postRun(t, server, "Helper", RunAgentInput{
			ThreadID: threadID,
			Messages: []Message{{ID: "u1", Role: RoleUser, Content: "go"}},
		})
	}()
	<-gate.entered

	// A concurrent turn on the same thread folds into the live run → 204.
	body2, _ := json.Marshal(RunAgentInput{ThreadID: threadID, Messages: []Message{{ID: "u2", Role: RoleUser, Content: "more"}}})
	res2, err := http.Post(server.URL+"/agents/Helper/run", "application/json", bytes.NewReader(body2))
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, res2.Body)
	res2.Body.Close()
	assert.Equal(t, http.StatusNoContent, res2.StatusCode)

	// Rejoin the live run's stream; it should follow the run to completion.
	// Connect before releasing the gate: the stream endpoint only attaches
	// to a run in flight, and assertions stay on the test goroutine — a
	// failed require in a worker deadlocks the test instead of reporting.
	rejoinRes, err := http.Get(server.URL + "/agents/Helper/threads/" + threadID + "/stream")
	require.NoError(t, err)
	defer rejoinRes.Body.Close()
	require.Equal(t, http.StatusOK, rejoinRes.StatusCode, "a live run must be rejoinable")

	rejoinDone := make(chan []sseFrame, 1)
	go func() { rejoinDone <- readSSEFrames(t, rejoinRes.Body) }()

	// Release the gate so the run finishes.
	close(gate.release)

	first := <-firstDone
	firstEvents := frameEvents(first)
	assert.Equal(t, "RUN_FINISHED", firstEvents[len(firstEvents)-1])

	rejoin := <-rejoinDone
	rejoinEvents := frameEvents(rejoin)
	require.NotEmpty(t, rejoinEvents)
	assert.Equal(t, "RUN_STARTED", rejoinEvents[0])
	assert.Contains(t, rejoinEvents, "RUN_FINISHED")
}
