package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedStep is one LLM turn: chunks streamed through the callback,
// then the accumulated response returned to the agent loop.
type scriptedStep struct {
	chunks   []*responses.ResponseChunk
	response *responses.Response
}

type scriptedLLM struct {
	mu    sync.Mutex
	steps []scriptedStep
	calls int
}

func (s *scriptedLLM) NewStreamingResponses(ctx context.Context, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.steps) {
		return nil, fmt.Errorf("scripted LLM exhausted after %d calls", s.calls)
	}
	step := s.steps[s.calls]
	s.calls++
	for _, chunk := range step.chunks {
		cb(chunk)
	}
	return step.response, nil
}

type approvalTool struct {
	*agents.BaseTool
	output string
}

func newApprovalTool(name, output string) *approvalTool {
	return &approvalTool{
		BaseTool: &agents.BaseTool{
			RequiresApproval: true,
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        name,
					Description: utils.Ptr("test tool"),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
		output: output,
	}
}

func (t *approvalTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(t.output)},
		},
	}, nil
}

func assistantTextResponse(text string) *responses.Response {
	return &responses.Response{
		Output: []responses.OutputMessageUnion{{
			OfOutputMessage: &responses.OutputMessage{
				ID:   responses.NewOutputItemMessageID(),
				Role: constants.RoleAssistant,
				Content: &responses.OutputContent{
					{OfOutputText: &responses.OutputTextContent{Text: text}},
				},
			},
		}},
	}
}

func toolCallResponse(callID, name, args string) *responses.Response {
	return &responses.Response{
		Output: []responses.OutputMessageUnion{{
			OfFunctionCall: &responses.FunctionCallMessage{
				ID:        "fc_" + callID,
				CallID:    callID,
				Name:      name,
				Arguments: args,
			},
		}},
	}
}

type registry map[string]*agents.Agent

func (r registry) Agent(name string) (*agents.Agent, bool) {
	a, ok := r[name]
	return a, ok
}

func (r registry) AgentNames() []string {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	return names
}

type sseFrame struct {
	event string
	data  map[string]any
}

func postRun(t *testing.T, server *httptest.Server, agentName string, input RunAgentInput) []sseFrame {
	t.Helper()
	body, err := json.Marshal(input)
	require.NoError(t, err)

	res, err := http.Post(server.URL+"/agents/"+agentName+"/run", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "text/event-stream", res.Header.Get("Content-Type"))

	var buf bytes.Buffer
	_, err = buf.ReadFrom(res.Body)
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

func frameEvents(frames []sseFrame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.event)
	}
	return out
}

func findFrame(frames []sseFrame, event string) (sseFrame, bool) {
	for _, f := range frames {
		if f.event == event {
			return f, true
		}
	}
	return sseFrame{}, false
}

func TestAgentsEndpoint(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(&scriptedLLM{})
	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	res, err := http.Get(server.URL + "/agents")
	require.NoError(t, err)
	defer res.Body.Close()

	var payload struct {
		Agents []string `json:"agents"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&payload))
	assert.Equal(t, []string{"Helper"}, payload.Agents)
}

func TestRunRejectsBadInput(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(&scriptedLLM{})
	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	res, err := http.Post(server.URL+"/agents/Helper/run", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)) // no threadId
	require.NoError(t, err)
	defer res.Body.Close()
	assert.Equal(t, http.StatusBadRequest, res.StatusCode)

	res, err = http.Post(server.URL+"/agents/Nope/run", "application/json",
		strings.NewReader(`{"threadId":"t","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	defer res.Body.Close()
	assert.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestEndToEndTextRun(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{{
		chunks: []*responses.ResponseChunk{
			messageAdded("msg_1"),
			textDelta("msg_1", "Hello "),
			textDelta("msg_1", "there!"),
			messageDone("msg_1"),
		},
		response: assistantTextResponse("Hello there!"),
	}}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "Helper"}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"Helper": agent}))
	defer server.Close()

	frames := postRun(t, server, "Helper", RunAgentInput{
		ThreadID: "thread-1",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "hi"}},
	})

	events := frameEvents(frames)
	assert.Equal(t, "RUN_STARTED", events[0])
	assert.Equal(t, "RUN_FINISHED", events[len(events)-1])

	// The text streamed through with correct bracketing.
	assert.Contains(t, events, "TEXT_MESSAGE_START")
	assert.Contains(t, events, "TEXT_MESSAGE_END")
	text := ""
	for _, f := range frames {
		if f.event == "TEXT_MESSAGE_CONTENT" {
			text += f.data["delta"].(string)
		}
	}
	assert.Equal(t, "Hello there!", text)

	finished, ok := findFrame(frames, "RUN_FINISHED")
	require.True(t, ok)
	assert.Equal(t, "completed", finished.data["result"].(map[string]any)["status"])
}

func TestEndToEndApprovalFlow(t *testing.T) {
	llm := &scriptedLLM{steps: []scriptedStep{
		{
			chunks: []*responses.ResponseChunk{
				functionCallAdded("item-1", "call-1", "delete_user"),
				argsDelta("item-1", `{"user_id":"123"}`),
			},
			response: toolCallResponse("call-1", "delete_user", `{"user_id":"123"}`),
		},
		{
			chunks: []*responses.ResponseChunk{
				messageAdded("msg_1"),
				textDelta("msg_1", "User 123 deleted."),
				messageDone("msg_1"),
			},
			response: assistantTextResponse("User 123 deleted."),
		},
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "UserManager",
		Tools: []agents.Tool{newApprovalTool("delete_user", "deleted user 123")},
	}).WithLLM(llm)
	server := httptest.NewServer(NewHandler(registry{"UserManager": agent}))
	defer server.Close()

	// First POST: the run pauses for approval.
	frames := postRun(t, server, "UserManager", RunAgentInput{
		ThreadID: "thread-1",
		Messages: []Message{{ID: "u1", Role: RoleUser, Content: "delete user 123"}},
	})

	interrupt, ok := findFrame(frames, "CUSTOM")
	require.True(t, ok)
	// First CUSTOM is the stream id; find the interrupt specifically.
	for _, f := range frames {
		if f.event == "CUSTOM" && f.data["name"] == CustomNameInterrupt {
			interrupt = f
		}
	}
	require.Equal(t, CustomNameInterrupt, interrupt.data["name"])
	value := interrupt.data["value"].(map[string]any)
	pending := value["pendingToolCalls"].([]any)
	require.Len(t, pending, 1)
	assert.Equal(t, "call-1", pending[0].(map[string]any)["toolCallId"])

	finished, ok := findFrame(frames, "RUN_FINISHED")
	require.True(t, ok)
	assert.Equal(t, "paused", finished.data["result"].(map[string]any)["status"])

	// Second POST: approval-only resume on the same thread.
	frames = postRun(t, server, "UserManager", RunAgentInput{
		ThreadID: "thread-1",
		ForwardedProps: map[string]any{
			"command": map[string]any{
				"resume": map[string]any{
					"decisions": []any{map[string]any{"toolCallId": "call-1", "approved": true}},
				},
			},
		},
	})

	result, ok := findFrame(frames, "TOOL_CALL_RESULT")
	require.True(t, ok)
	assert.Equal(t, "deleted user 123", result.data["content"])

	text := ""
	for _, f := range frames {
		if f.event == "TEXT_MESSAGE_CONTENT" {
			text += f.data["delta"].(string)
		}
	}
	assert.Equal(t, "User 123 deleted.", text)

	finished, ok = findFrame(frames, "RUN_FINISHED")
	require.True(t, ok)
	assert.Equal(t, "completed", finished.data["result"].(map[string]any)["status"])
}
