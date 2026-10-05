package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

// answeringLLM answers every call with text, and keeps the requests.
type answeringLLM struct {
	mu       sync.Mutex
	text     string
	requests []*responses.Request
}

func (l *answeringLLM) NewStreamingResponses(_ context.Context, _ *ModelCall, req *responses.Request, _ func(*responses.ResponseChunk)) (*responses.Response, error) {
	l.mu.Lock()
	l.requests = append(l.requests, req)
	l.mu.Unlock()
	return &responses.Response{Output: []responses.OutputMessageUnion{{OfOutputMessage: &responses.OutputMessage{
		ID: responses.NewOutputItemMessageID(), Role: constants.RoleAssistant,
		Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: l.text}}},
	}}}}, nil
}

// mapSubAgents serves named agents, and the caller when self is set, the way
// a registry-backed client does.
type mapSubAgents struct {
	agents map[string]*Agent
	self   bool
}

func (c *mapSubAgents) ListSubAgents(_ context.Context, query SubAgentQuery) ([]SubAgentInfo, error) {
	var listed []SubAgentInfo
	if c.self {
		listed = append(listed, SubAgentInfo{Name: query.Caller, Self: true})
	}
	names := slices.Sorted(maps.Keys(c.agents))
	for _, name := range names {
		listed = append(listed, SubAgentInfo{Name: name, Description: c.agents[name].Description()})
	}
	return listed, nil
}

func (c *mapSubAgents) resolve(req SubAgentRequest) (*Agent, error) {
	if c.self && req.Name == req.Caller.Name {
		return req.Caller, nil
	}
	if agent, ok := c.agents[req.Name]; ok {
		return agent, nil
	}
	return nil, fmt.Errorf("no sub-agent %q", req.Name)
}

func (c *mapSubAgents) RunSubAgent(ctx context.Context, req SubAgentRequest) (AgentTaskOutcome, error) {
	agent, err := c.resolve(req)
	if err != nil {
		return AgentTaskOutcome{}, err
	}
	return RunAgentTask(ctx, agent, req.Input)
}

func (c *mapSubAgents) SteerSubAgent(ctx context.Context, req SubAgentRequest) (bool, error) {
	agent, err := c.resolve(req)
	if err != nil {
		return false, err
	}
	return SteerLocalSubAgent(ctx, agent, req)
}

// SubAgents gives an agent call_sub_agent. Called async on itself, it hands
// work to a copy of the agent, which works in a hidden thread under the caller
// and whose answer comes back on its own; follow-ups go to the same thread.
func TestCallSubAgentHandsWorkToACopyOfTheAgent(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	llm := &answeringLLM{text: "sub-agent answer"}
	options := &AgentOptions{
		Name:         "assistant",
		History:      history.NewConversationManager(store),
		StreamBroker: streambroker.NewMemoryStreamBroker(),
		SubAgents:    &mapSubAgents{self: true},
	}
	agent := NewAgent(options).WithLLM(llm)

	// Recorded on the options, where a durable runtime registers tools from.
	require.Len(t, options.Tools, 1)
	require.Equal(t, CallSubAgentToolName, functionName(options.Tools[0]))

	// The agent's own tool calls as the agent it is, model and all.
	require.Len(t, agent.tools, 1)
	tool := agent.tools[0].(*CallSubAgentTool)
	require.Same(t, agent, tool.agent, "WithLLM re-points the tool at the copy it returns")

	// The caller's thread holds a message the sub-agent's history must not start with.
	require.NoError(t, store.SaveMessages(ctx, "tenant", "", "", false, "parent-run", "", "parent", "parent-session", []history.Message{
		messages.NewWithID("m1", "user", []responses.InputMessageUnion{responses.UserMessage("caller secret")}),
	}, nil))

	call := subAgentCall(`{"agent":"assistant","mode":"async","message":"summarize the doc"}`, "call_1")
	resp, err := tool.Execute(ctx, call)
	require.NoError(t, err)
	require.NotEmpty(t, resp.TaskID, "it answers at once and leaves a task to wait on")
	threadID := subAgentThreadID(t, *resp.Output.OfString)

	answer := awaitSubAgent(t, tool, resp, call)
	require.Contains(t, answer, "sub-agent answer")
	require.Equal(t, threadID, subAgentThreadID(t, answer))

	threads, err := store.ListThreads(ctx, "tenant", "parent")
	require.NoError(t, err)
	require.Len(t, threads, 1)
	require.Equal(t, threadID, threads[0].ThreadID)
	require.Equal(t, "parent", threads[0].GroupID)
	require.Equal(t, "parent", threads[0].ParentThreadID)
	require.True(t, threads[0].Hidden)

	// The sub-agent's model saw its task and nothing of the caller's thread.
	require.Len(t, llm.requests, 1)
	input, err := json.Marshal(llm.requests[0].Input)
	require.NoError(t, err)
	require.Contains(t, string(input), "summarize the doc")
	require.NotContains(t, string(input), "caller secret")

	// A follow-up goes to the same thread, and its answer comes back the same way.
	followUp := subAgentCall(`{"agent":"assistant","mode":"async","message":"now shorter","thread_id":"`+threadID+`"}`, "call_2")
	resp, err = tool.Execute(ctx, followUp)
	require.NoError(t, err)
	require.Equal(t, threadID, subAgentThreadID(t, *resp.Output.OfString))
	require.Contains(t, awaitSubAgent(t, tool, resp, followUp), "sub-agent answer")
	rows, err := history.LoadTranscript(ctx, store, "tenant", threadID)
	require.NoError(t, err)
	require.Len(t, rows, 2, "two turns on one sub-agent thread")

	// The sub-agent's thread goes into the group the call names for new threads.
	grouped := subAgentCall(`{"agent":"assistant","mode":"async","message":"elsewhere"}`, "call_grouped")
	grouped.GroupID = "group-from-call"
	resp, err = tool.Execute(ctx, grouped)
	require.NoError(t, err)
	awaitSubAgent(t, tool, resp, grouped)
	inGroup, err := store.ListThreads(ctx, "tenant", "group-from-call")
	require.NoError(t, err)
	require.Len(t, inGroup, 1)
	require.Equal(t, "parent", inGroup[0].ParentThreadID)

	// While the sub-agent is at work, a follow-up joins its current task, in
	// either mode.
	broker := agent.StreamBroker()
	stream := StreamIDForThread("tenant", threadID)
	started, err := broker.(RunClaimBroker).EnqueueOrStart(ctx, stream, nil)
	require.NoError(t, err)
	require.True(t, started, "stand in for a run in flight")
	steered, err := tool.Execute(ctx, subAgentCall(`{"agent":"assistant","mode":"sync","message":"one more thing","thread_id":"`+threadID+`"}`, "call_3"))
	require.NoError(t, err)
	require.Empty(t, steered.TaskID)
	require.Contains(t, *steered.Output.OfString, "added to it")
	queued, err := broker.DrainMessages(ctx, stream)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.NoError(t, broker.Close(ctx, stream))
}

// In sync mode the call waits, and the sub-agent's answer is its result.
func TestCallSubAgentSyncWaitsForTheAnswer(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	writer := NewAgent(&AgentOptions{
		Name:        "writer",
		Description: "Writes things.",
		History:     history.NewConversationManager(store),
	}).WithLLM(&answeringLLM{text: "a short poem"})
	agent := NewAgent(&AgentOptions{
		Name:      "assistant",
		History:   history.NewConversationManager(store),
		SubAgents: &mapSubAgents{agents: map[string]*Agent{"writer": writer}},
	}).WithLLM(&answeringLLM{text: "unused"})
	tool := agent.tools[0].(*CallSubAgentTool)

	call := subAgentCall(`{"agent":"writer","mode":"sync","message":"write a poem"}`, "call_1")
	resp, err := tool.Execute(ctx, call)
	require.NoError(t, err)
	require.Empty(t, resp.TaskID, "nothing is left to wait on")
	require.Contains(t, *resp.Output.OfString, "a short poem")
	threadID := subAgentThreadID(t, *resp.Output.OfString)

	threads, err := store.ListThreads(ctx, "tenant", "parent")
	require.NoError(t, err)
	require.Len(t, threads, 1)
	require.Equal(t, threadID, threads[0].ThreadID)
	require.True(t, threads[0].Hidden)

	// The same call again — a retry — finds its turn rather than running it twice.
	resp, err = tool.Execute(ctx, call)
	require.NoError(t, err)
	require.Contains(t, *resp.Output.OfString, "a short poem")
	rows, err := history.LoadTranscript(ctx, store, "tenant", threadID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

// A call is checked against the listing: an agent the client does not list,
// the caller included without self, is refused before anything runs.
func TestCallSubAgentRefusesAnAgentItIsNotGiven(t *testing.T) {
	ctx := context.Background()
	writer := NewAgent(&AgentOptions{Name: "writer"}).WithLLM(&answeringLLM{text: "unused"})
	agent := NewAgent(&AgentOptions{
		Name:      "assistant",
		SubAgents: &mapSubAgents{agents: map[string]*Agent{"writer": writer}},
	}).WithLLM(&answeringLLM{text: "unused"})
	tool := agent.tools[0].(*CallSubAgentTool)

	_, err := tool.Execute(ctx, subAgentCall(`{"agent":"assistant","mode":"sync","message":"hi"}`, "call_1"))
	require.ErrorContains(t, err, `no sub-agent named "assistant"; call one of: writer`)

	_, err = tool.Execute(ctx, subAgentCall(`{"agent":"writer","mode":"later","message":"hi"}`, "call_2"))
	require.ErrorContains(t, err, "mode must be")
}

// promptRecorder keeps the Dependencies each prompt was built from.
type promptRecorder struct {
	mu   sync.Mutex
	deps []*Dependencies
}

func (p *promptRecorder) GetPrompt(_ context.Context, deps *Dependencies) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deps = append(p.deps, deps)
	return "prompt", nil
}

// Each run lists its sub-agents for the prompt; a run with none to call is
// not offered the tool.
func TestRunListsSubAgentsForThePrompt(t *testing.T) {
	ctx := context.Background()
	writer := NewAgent(&AgentOptions{Name: "writer", Description: "Writes things."}).WithLLM(&answeringLLM{text: "unused"})
	client := &mapSubAgents{agents: map[string]*Agent{"writer": writer}, self: true}
	prompt := &promptRecorder{}
	llm := &answeringLLM{text: "done"}
	agent := NewAgent(&AgentOptions{Name: "assistant", Instruction: prompt, SubAgents: client}).WithLLM(llm)

	_, err := agent.Run(ctx, &AgentInput{Namespace: "tenant", Message: messages.New("user", []responses.InputMessageUnion{responses.UserMessage("hi")})})
	require.NoError(t, err)
	require.Equal(t, []SubAgentInfo{{Name: "assistant", Self: true}, {Name: "writer", Description: "Writes things."}}, prompt.deps[0].SubAgents)
	require.True(t, slices.ContainsFunc(llm.requests[0].Tools, func(tool responses.ToolUnion) bool {
		return tool.OfFunction != nil && tool.OfFunction.Name == CallSubAgentToolName
	}))

	client.agents, client.self = nil, false
	_, err = agent.Run(ctx, &AgentInput{Namespace: "tenant", Message: messages.New("user", []responses.InputMessageUnion{responses.UserMessage("hi")})})
	require.NoError(t, err)
	require.Empty(t, prompt.deps[1].SubAgents)
	require.Empty(t, llm.requests[1].Tools)
}

func subAgentCall(args, callID string) *ToolCall {
	return &ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{ID: "fc_" + callID, CallID: callID, Name: CallSubAgentToolName, Arguments: args},
		AgentName:           "assistant",
		Namespace:           "tenant",
		ThreadID:            "parent",
		SessionID:           "parent-session",
	}
}

func subAgentThreadID(t *testing.T, output string) string {
	t.Helper()
	_, id, ok := strings.Cut(output, "Thread ID: ")
	require.True(t, ok, "no thread id in %q", output)
	return strings.TrimSpace(id)
}

func awaitSubAgent(t *testing.T, tool *CallSubAgentTool, resp *ToolCallResponse, caller *ToolCall) string {
	t.Helper()
	result, err := tool.AwaitTask(context.Background(), BackgroundTaskRef{
		TaskID: resp.TaskID, AgentName: caller.AgentName, Namespace: caller.Namespace,
		ThreadID: caller.ThreadID, SessionID: caller.SessionID, Payload: resp.TaskPayload,
	}, nil)
	require.NoError(t, err)
	return *result.Output.Output.OfString
}

// A run's answer reads the same after its output crossed a durable runtime as
// JSON, where an assistant message decodes as an easy input message — or a
// sub-agent's answer delivered back from Temporal or Restate would be empty.
func TestAgentOutputTextSurvivesAJSONRoundTrip(t *testing.T) {
	out := &AgentOutput{Output: []responses.InputMessageUnion{{OfOutputMessage: &responses.OutputMessage{
		ID: "msg_1", Role: constants.RoleAssistant,
		Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: "the answer"}}},
	}}}}
	require.Equal(t, "the answer", out.Text())

	data, err := json.Marshal(out)
	require.NoError(t, err)
	var crossed AgentOutput
	require.NoError(t, json.Unmarshal(data, &crossed))
	require.Equal(t, "the answer", crossed.Text())
}
