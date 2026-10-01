package agents_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
)

var (
	_ agents.StreamBroker = (*streambroker.MemoryStreamBroker)(nil)
	_ agents.StreamBroker = (*streambroker.RedisStreamBroker)(nil)
)

var selectionTool = []agents.ClientToolDefinition{{
	Name:        "get_selection",
	Description: "Read the text the user has selected in the page.",
	Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
}}

// toolOutputMessage is what an AG-UI client sends after running its own tool.
// A live run's waiting tool takes it through the broker; a paused call is
// answered with clientToolResult instead.
func toolOutputMessage(callID, output string) history.Message {
	return messages.New("user", []responses.InputMessageUnion{{
		OfFunctionCallOutput: &responses.FunctionCallOutputMessage{
			CallID: callID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(output)},
		},
	}})
}

func requestJSON(t *testing.T, llm *scriptedLLM, i int) string {
	t.Helper()
	data, err := json.Marshal(llm.request(i))
	require.NoError(t, err)
	return string(data)
}

// pausingAgent pauses on client tools at once, as for a client that answers
// only after the run has paused.
func pausingAgent(llm *scriptedLLM, broker agents.StreamBroker) *agents.Agent {
	return agents.NewAgent(&agents.AgentOptions{
		Name: "main", StreamBroker: broker,
		ClientTools: agents.ClientToolOptions{Timeout: -1},
	}).WithLLM(llm)
}

// clientToolResult resumes a paused client tool with the client's result, as
// any interrupt is resumed.
func clientToolResult(callID, result string) history.Message {
	content, _ := json.Marshal(result)
	return elicitationMessage(callID, string(content))
}

func TestClientToolPauseResumesWithTheClientsResult(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_sel", "get_selection", "{}"),
		textResponse("summarized"),
	}}
	agent := pausingAgent(llm, streambroker.NewMemoryStreamBroker())
	in := func(runID string, msg history.Message) *agents.AgentInput {
		return &agents.AgentInput{Namespace: "test", ThreadID: "thread-client", PreviousRunID: runID, Message: msg, ClientTools: selectionTool}
	}
	out := runAgent(t, agent, in("", userMessage("summarize my selection")))

	// The model sees the client's tool, and the run pauses on the call.
	require.Contains(t, requestJSON(t, llm, 0), "get_selection")
	requireStatus(t, out, agentstate.RunStatusPaused)
	require.Len(t, out.Interrupts, 1)
	require.Equal(t, responses.InterruptModeClientTool, out.Interrupts[0].Mode)
	require.Equal(t, "get_selection", out.Interrupts[0].FunctionCallMessage.Name)

	// A bare tool output does not resume a pause: it is not a resolution.
	out = runAgent(t, agent, in(out.RunID, toolOutputMessage("call_sel", "unasked")))
	requireStatus(t, out, agentstate.RunStatusPaused)
	require.Equal(t, 1, llm.callCount())

	// The client's result, as a resolution, becomes the tool's output for the model.
	out = runAgent(t, agent, in(out.RunID, clientToolResult("call_sel", "Quarterly revenue grew 12%")))
	requireStatus(t, out, agentstate.RunStatusCompleted)
	request := requestJSON(t, llm, 1)
	require.Contains(t, request, "Quarterly revenue grew 12%")
	require.NotContains(t, request, "unasked")
	require.Contains(t, messagesText(out.Output), "Quarterly revenue grew 12%")
}

func TestClientToolDeclinedByTheClient(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_sel", "get_selection", "{}"),
		textResponse("ok"),
	}}
	agent := pausingAgent(llm, streambroker.NewMemoryStreamBroker())
	out := runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "thread-decline", Message: userMessage("go"), ClientTools: selectionTool})
	requireStatus(t, out, agentstate.RunStatusPaused)
	out = runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "thread-decline", PreviousRunID: out.RunID, Message: approvalMessage(nil, []string{"call_sel"}), ClientTools: selectionTool})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	// The loop answers a rejected call itself; the client tool never runs.
	require.Contains(t, requestJSON(t, llm, 1), "User has declined the request to call this tool")
}

func TestServerToolsWinClientToolNameCollisions(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_sel", "get_selection", "{}"),
		textResponse("done"),
	}}
	server := newFakeTool("get_selection", false, "from the server")
	agent := newScriptedAgent("main", llm, nil, nil, []agents.Tool{server}, nil)
	out := runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "thread-collide", Message: userMessage("go"), ClientTools: selectionTool})

	// The client's definition is dropped, so the model sees one tool and the server runs it.
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, 1, server.callCount())
	require.Len(t, llm.request(0).Tools, 1)
}

func TestClientToolNamesAreValidated(t *testing.T) {
	agent := newScriptedAgent("main", &scriptedLLM{}, nil, nil, nil, nil)
	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test", ThreadID: "thread-invalid", Message: userMessage("go"),
		ClientTools: []agents.ClientToolDefinition{{Name: "not a name"}},
	})
	require.NoError(t, err)
	_, err = handle.Result()
	require.ErrorContains(t, err, "invalid client tool name")
}

func TestClientToolWaitsForTheResult(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	newAgent := func(llm *scriptedLLM, timeout time.Duration) *agents.Agent {
		return agents.NewAgent(&agents.AgentOptions{
			Name: "main", StreamBroker: broker,
			ClientTools: agents.ClientToolOptions{Timeout: timeout},
		}).WithLLM(llm)
	}

	// A result sent before the tool starts waiting is still found, and the run
	// never pauses. The first output for a call wins.
	require.NoError(t, broker.EnqueueMessage(t.Context(), "stream-wait", toolOutputMessage("call_sel", "Quarterly revenue grew 12%")))
	require.NoError(t, broker.EnqueueMessage(t.Context(), "stream-wait", toolOutputMessage("call_sel", "second answer")))

	llm := &scriptedLLM{script: []*responses.Response{toolCallResponse("call_sel", "get_selection", "{}"), textResponse("summarized")}}
	out := runAgent(t, newAgent(llm, time.Minute), &agents.AgentInput{Namespace: "test", ThreadID: "thread-wait", StreamID: "stream-wait", Message: userMessage("go"), ClientTools: selectionTool})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Contains(t, requestJSON(t, llm, 1), "Quarterly revenue grew 12%")
	require.NotContains(t, requestJSON(t, llm, 1), "second answer")

	// A result sent while the tool waits wakes it.
	llm = &scriptedLLM{script: []*responses.Response{toolCallResponse("call_live", "get_selection", "{}"), textResponse("summarized")}}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = broker.EnqueueMessage(context.Background(), "stream-live", toolOutputMessage("call_live", "live answer"))
	}()
	out = runAgent(t, newAgent(llm, time.Minute), &agents.AgentInput{Namespace: "test", ThreadID: "thread-live", StreamID: "stream-live", Message: userMessage("go"), ClientTools: selectionTool})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Contains(t, requestJSON(t, llm, 1), "live answer")

	// Without an answer in time, the run pauses on the call for a later one.
	llm = &scriptedLLM{script: []*responses.Response{toolCallResponse("call_none", "get_selection", "{}"), textResponse("gave up")}}
	out = runAgent(t, newAgent(llm, 50*time.Millisecond), &agents.AgentInput{Namespace: "test", ThreadID: "thread-silent", StreamID: "stream-silent", Message: userMessage("go"), ClientTools: selectionTool})
	requireStatus(t, out, agentstate.RunStatusPaused)
	require.Len(t, out.Interrupts, 1)
	require.Equal(t, responses.InterruptModeClientTool, out.Interrupts[0].Mode)
	require.Equal(t, 1, llm.callCount(), "the model is not told anything until the client answers")
}

// Even an agent that pauses at once takes a result the client sent before the
// tool ran, instead of pausing for it.
func TestClientToolTakesAnEarlyResultWithoutWaiting(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	llm := &scriptedLLM{script: []*responses.Response{toolCallResponse("call_sel", "get_selection", "{}"), textResponse("summarized")}}
	agent := pausingAgent(llm, broker)
	require.NoError(t, broker.EnqueueMessage(t.Context(), "stream-early", toolOutputMessage("call_sel", "early answer")))

	out := runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "thread-early", StreamID: "stream-early", Message: userMessage("go"), ClientTools: selectionTool})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Contains(t, requestJSON(t, llm, 1), "early answer")
}

// A tool output for a call the thread is not waiting on — a result after its
// run moved on — is dropped, and a run given nothing else does not call the model.
func TestStaleToolOutputStartsNoModelCall(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("hello")}}
	agent := newScriptedAgent("main", llm, nil, nil, nil, nil)
	out := runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "thread-stale", Message: userMessage("hi")})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	out = runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "thread-stale", PreviousRunID: out.RunID, Message: toolOutputMessage("call_gone", "late")})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	require.Equal(t, 1, llm.callCount(), "the stale output reached no model")
}

// toolCallsResponse is a model reply that calls several tools at once.
func toolCallsResponse(calls ...[2]string) *responses.Response {
	out := &responses.Response{}
	for _, c := range calls {
		out.Output = append(out.Output, responses.OutputMessageUnion{OfFunctionCall: &responses.FunctionCallMessage{
			ID: "fc_" + c[0], CallID: c[0], Name: c[1], Arguments: "{}",
		}})
	}
	return out
}

// A tool message cannot answer a paused server tool: the call is answered by
// approving it, and the model sees the tool's own output, once.
func TestToolMessageCannotAnswerAPausedServerTool(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{toolCallResponse("call_srv", "danger", "{}"), textResponse("done")}}
	agent := newScriptedAgent("main", llm, nil, nil, []agents.Tool{newFakeTool("danger", true, "REAL_OUTPUT")}, nil)
	in := func(runID string, msg history.Message) *agents.AgentInput {
		return &agents.AgentInput{Namespace: "test", ThreadID: "thread-fake", PreviousRunID: runID, Message: msg}
	}

	out := runAgent(t, agent, in("", userMessage("go")))
	requireStatus(t, out, agentstate.RunStatusPaused)
	out = runAgent(t, agent, in(out.RunID, toolOutputMessage("call_srv", "FABRICATED")))
	requireStatus(t, out, agentstate.RunStatusPaused)
	out = runAgent(t, agent, in(out.RunID, approvalMessage([]string{"call_srv"}, nil)))
	requireStatus(t, out, agentstate.RunStatusCompleted)

	request := requestJSON(t, llm, 1)
	require.NotContains(t, request, "FABRICATED")
	require.Equal(t, 1, strings.Count(request, "REAL_OUTPUT"))
}

// A pause that holds an approval and a client tool: answering only the
// approval runs the approved tool, and the client tool pauses again rather
// than answering with nothing. Its result, sent later, completes the run.
func TestMixedPauseWaitsForTheClientToolsResult(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallsResponse([2]string{"call_srv", "danger"}, [2]string{"call_sel", "get_selection"}),
		textResponse("done"),
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name: "main", StreamBroker: streambroker.NewMemoryStreamBroker(),
		Tools:       []agents.Tool{newFakeTool("danger", true, "REAL_OUTPUT")},
		ClientTools: agents.ClientToolOptions{Timeout: -1},
	}).WithLLM(llm)
	in := func(runID string, msg history.Message) *agents.AgentInput {
		return &agents.AgentInput{Namespace: "test", ThreadID: "thread-mixed", PreviousRunID: runID, Message: msg, ClientTools: selectionTool}
	}

	out := runAgent(t, agent, in("", userMessage("go")))
	requireStatus(t, out, agentstate.RunStatusPaused)

	out = runAgent(t, agent, in(out.RunID, approvalMessage([]string{"call_srv"}, nil)))
	requireStatus(t, out, agentstate.RunStatusPaused)
	require.Len(t, out.Interrupts, 1)
	require.Equal(t, "call_sel", out.Interrupts[0].FunctionCallMessage.CallID)
	require.Equal(t, responses.InterruptModeClientTool, out.Interrupts[0].Mode)
	require.Equal(t, 1, llm.callCount(), "the model waits for both results")

	out = runAgent(t, agent, in(out.RunID, clientToolResult("call_sel", "selected text")))
	requireStatus(t, out, agentstate.RunStatusCompleted)
	request := requestJSON(t, llm, 1)
	require.Contains(t, request, "selected text")
	require.Equal(t, 1, strings.Count(request, "REAL_OUTPUT"))
	require.Equal(t, 2, strings.Count(request, "function_call_output"), "one output for each call")
}

// A run resumed by a request that does not define the client's tools again —
// a result sent after a page reload, an approval answered from another client —
// still gives the paused call the client's answer.
func TestResumeWithoutClientToolDefinitions(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{toolCallResponse("call_sel", "get_selection", "{}"), textResponse("done")}}
	agent := pausingAgent(llm, streambroker.NewMemoryStreamBroker())
	out := runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "thread-undefined", Message: userMessage("go"), ClientTools: selectionTool})
	requireStatus(t, out, agentstate.RunStatusPaused)

	out = runAgent(t, agent, &agents.AgentInput{Namespace: "test", ThreadID: "thread-undefined", PreviousRunID: out.RunID, Message: clientToolResult("call_sel", "selected text")})
	requireStatus(t, out, agentstate.RunStatusCompleted)
	request := requestJSON(t, llm, 1)
	require.Contains(t, request, "selected text")
	require.NotContains(t, request, "Tool not found")
}

func TestClientToolOutputKeepsJSON(t *testing.T) {
	require.Equal(t, "text", agents.ClientToolOutput(json.RawMessage(`"text"`)))
	require.Equal(t, `{"rows":2}`, agents.ClientToolOutput(json.RawMessage(`{"rows":2}`)))
	require.Empty(t, agents.ClientToolOutput(nil))
}
