package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingTool runs until its context is cancelled.
type blockingTool struct {
	*agents.BaseTool
	started  chan struct{}
	finished chan struct{}
}

func newBlockingTool(name string) *blockingTool {
	return &blockingTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        name,
					Description: utils.Ptr("test tool"),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
		started:  make(chan struct{}),
		finished: make(chan struct{}),
	}
}

func (t *blockingTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	defer close(t.finished)
	close(t.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

// startRun posts a run and returns the live response, so the test can
// read the stream id and act on the run while it is still going.
func startRun(t *testing.T, server *httptest.Server, agentName string, input RunAgentInput) *http.Response {
	t.Helper()
	body, err := json.Marshal(input)
	require.NoError(t, err)

	res, err := http.Post(server.URL+"/agents/"+agentName+"/run", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	return res
}

func postStop(t *testing.T, server *httptest.Server, agentName, streamID string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"streamId": streamID})
	require.NoError(t, err)

	res, err := http.Post(server.URL+"/agents/"+agentName+"/stop", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	return res
}

// parseFrames reads an SSE body to completion and returns its frames,
// mirroring postRun's parsing.
func parseFrames(t *testing.T, res *http.Response) []sseFrame {
	t.Helper()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(res.Body)
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

// A stop on its own request must reach the run streamed on another, cut
// the tool call short, and still finish the stream cleanly.
func TestStopEndpointEndsRunInFlight(t *testing.T) {
	tool := newBlockingTool("slow")

	// One scripted turn: a second LLM call would exhaust the script.
	llm := &scriptedLLM{steps: []scriptedStep{{
		response: toolCallResponse("call_slow", "slow", "{}"),
	}}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "Helper",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	res := startRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-stop",
		Messages: []Message{{ID: "m1", Role: "user", Content: "start something slow"}},
	})
	defer res.Body.Close()

	streamID := res.Header.Get("X-Stream-Id")
	require.NotEmpty(t, streamID, "run must disclose its stream id for the stop endpoint")

	// Only once the tool is running, so this exercises a call in flight.
	select {
	case <-tool.started:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started")
	}

	stopRes := postStop(t, server, "Helper", streamID)
	defer stopRes.Body.Close()
	require.Equal(t, http.StatusAccepted, stopRes.StatusCode)

	// The run ends on its own connection, cleanly.
	frames := parseFrames(t, res)
	events := frameEvents(frames)
	assert.Contains(t, events, "RUN_FINISHED", "stopped run must still finish its stream: %v", events)
	assert.NotContains(t, events, "RUN_ERROR", "stopping a run is not an error: %v", events)
	assert.Equal(t, 1, llm.calls, "the run kept going after the stop")
}

// The endpoint records the stop without claiming to know whether a run was
// there to receive it — across replicas it generally can't.
func TestStopEndpointAcceptsUnknownStream(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).
		WithLLM(&scriptedLLM{})

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	res := postStop(t, server, "Helper", "no-such-stream")
	defer res.Body.Close()
	assert.Equal(t, http.StatusAccepted, res.StatusCode)
}

func TestStopEndpointRequiresStreamID(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).
		WithLLM(&scriptedLLM{})

	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	res, err := http.Post(server.URL+"/agents/Helper/stop", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	defer res.Body.Close()
	assert.Equal(t, http.StatusBadRequest, res.StatusCode)
}

// The agent in the path decides whose broker hears the stop, so agents on
// separate brokers cannot stop each other's runs. This keeps the endpoint
// from drifting into a search across every registered agent.
func TestStopEndpointGoesToTheNamedAgentsBroker(t *testing.T) {
	tool := newBlockingTool("slow")
	llm := &scriptedLLM{steps: []scriptedStep{{
		response: toolCallResponse("call_slow", "slow", "{}"),
	}}}
	helper := agents.NewAgent(&agents.AgentOptions{
		Name:  "Helper",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)
	other := agents.NewAgent(&agents.AgentOptions{Name: "Other"}).
		WithLLM(&scriptedLLM{})

	server := httptest.NewServer(NewHandler(registry{"Helper": helper, "Other": other}))
	defer server.Close()

	res := startRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-cross-agent",
		Messages: []Message{{ID: "m1", Role: "user", Content: "start something slow"}},
	})
	defer res.Body.Close()

	streamID := res.Header.Get("X-Stream-Id")
	require.NotEmpty(t, streamID)
	select {
	case <-tool.started:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started")
	}

	// Recorded on Other's broker, which this run never reads.
	crossRes := postStop(t, server, "Other", streamID)
	defer crossRes.Body.Close()
	require.Equal(t, http.StatusAccepted, crossRes.StatusCode)

	select {
	case <-tool.finished:
		t.Fatal("run was stopped through an agent wired to a different broker")
	case <-time.After(200 * time.Millisecond):
	}

	// Through its own agent it stops.
	ownRes := postStop(t, server, "Helper", streamID)
	defer ownRes.Body.Close()
	require.Equal(t, http.StatusAccepted, ownRes.StatusCode)

	select {
	case <-tool.finished:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop through its own agent")
	}
	parseFrames(t, res)
}
