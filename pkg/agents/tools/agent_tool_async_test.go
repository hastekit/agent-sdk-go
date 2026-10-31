package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/tools"
	"github.com/stretchr/testify/require"
)

// threadIDFrom reads the thread id an agent tool appends to its output.
func threadIDFrom(t *testing.T, output string) string {
	t.Helper()
	_, id, ok := strings.Cut(output, "Thread ID: ")
	require.True(t, ok, "no thread id in %q", output)
	return strings.TrimSpace(id)
}

// awaitTask waits for an async agent tool call's task, as the runtime would.
func awaitTask(t *testing.T, tool agents.BackgroundTool, resp *agents.ToolCallResponse, caller *agents.ToolCall) string {
	t.Helper()
	result, err := tool.AwaitTask(context.Background(), agents.BackgroundTaskRef{
		TaskID: resp.TaskID, AgentName: caller.AgentName, Namespace: caller.Namespace,
		ThreadID: caller.ThreadID, SessionID: caller.SessionID, Payload: resp.TaskPayload,
	}, nil)
	require.NoError(t, err)
	return *result.Output.Output.OfString
}

// Whether a call waits is fixed by the constructor, not chosen by the model:
// NewAgentTool waits for the answer, NewAsyncAgentTool returns at once and
// leaves a task whose answer arrives later. Only the async one is a
// background tool.
func TestAgentToolConstructorDecidesWhetherCallsWait(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	sub := newTestAgent(t, "researcher", "found it", store)

	sync := tools.NewAgentTool("research", "Research a topic", sub, tools.SubAgentContextModeNone)
	_, isBackground := any(sync).(agents.BackgroundTool)
	require.False(t, isBackground)
	waited, err := sync.Execute(ctx, toolCall("research", `{"message":"look this up"}`, "parent"))
	require.NoError(t, err)
	require.Empty(t, waited.TaskID, "waited on: the answer is the result")
	require.True(t, strings.HasPrefix(*waited.Output.OfString, "found it"))

	async := tools.NewAsyncAgentTool("research", "Research a topic", sub, tools.SubAgentContextModeNone)
	require.NotContains(t, async.GetToolDescriptor().ToolUnion.OfFunction.Parameters["properties"], "async", "nothing for the model to choose")
	call := toolCall("research", `{"message":"and this"}`, "parent")
	call.CallID = "call_async"
	started, err := async.Execute(ctx, call)
	require.NoError(t, err)
	require.NotEmpty(t, started.TaskID)
	require.Contains(t, *started.Output.OfString, "working in the background")
	require.Contains(t, awaitTask(t, async, started, call), "found it")
}

// A follow-up to a sub-agent still at work is steered into its current task,
// as a user's message joins a busy conversation: no new task, and the
// answer to the current one covers it. One that got past that check while the
// run was starting joins it too.
func TestAsyncAgentToolSteersAFollowUpIntoWorkInProgress(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	sub := newTestAgent(t, "researcher", "found it", store)
	tool := tools.NewAsyncAgentTool("research", "Research a topic", sub, tools.SubAgentContextModeNone)

	call := toolCall("research", `{"message":"look this up"}`, "parent")
	resp, err := tool.Execute(ctx, call)
	require.NoError(t, err)
	threadID := threadIDFrom(t, awaitTask(t, tool, resp, call))

	// Stand in for a run in flight on the sub-agent's thread.
	broker := sub.StreamBroker()
	stream := agents.StreamIDForThread("tenant", threadID)
	started, err := broker.(agents.RunClaimBroker).EnqueueOrStart(ctx, stream, nil)
	require.NoError(t, err)
	require.True(t, started)

	followUp := toolCall("research", `{"message":"and this","thread_id":"`+threadID+`"}`, "parent")
	followUp.CallID = "call_and_this"
	steered, err := tool.Execute(ctx, followUp)
	require.NoError(t, err)
	require.Empty(t, steered.TaskID, "no new task: the current one's answer covers it")
	require.Contains(t, *steered.Output.OfString, "added to it")
	require.Equal(t, threadID, threadIDFrom(t, *steered.Output.OfString))

	// The race: the task is already started when the run begins.
	payload, err := json.Marshal(map[string]string{"thread_id": threadID, "message_id": "late", "sender_id": "assistant", "message": "late follow-up"})
	require.NoError(t, err)
	late := awaitTask(t, tool, &agents.ToolCallResponse{TaskID: "late", TaskPayload: payload}, followUp)
	require.Contains(t, late, "added to its current task")

	queued, err := broker.DrainMessages(ctx, stream)
	require.NoError(t, err)
	require.Len(t, queued, 2, "both follow-ups wait for the run's next step")
	require.NoError(t, broker.Close(ctx, stream))
}

// In isolated mode each calling thread keeps one sub-agent thread, without
// the model having to name it.
func TestAsyncAgentToolIsolatedModeKeepsOneThread(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	sub := newTestAgent(t, "researcher", "found it", store)
	tool := tools.NewAsyncAgentTool("research", "Research a topic", sub, tools.SubAgentContextModeIsolated)

	first := toolCall("research", `{"message":"one"}`, "parent")
	resp, err := tool.Execute(ctx, first)
	require.NoError(t, err)
	require.NotContains(t, *resp.Output.OfString, "Thread ID", "isolated mode keeps the thread to itself")
	state := resp.StateUpdates
	require.NotEmpty(t, awaitTask(t, tool, resp, first))

	second := toolCall("research", `{"message":"two"}`, "parent")
	second.CallID = "call_two"
	second.State = state
	resp, err = tool.Execute(ctx, second)
	require.NoError(t, err)
	require.Equal(t, state, resp.StateUpdates, "the same sub-agent thread again")
	awaitTask(t, tool, resp, second)

	threads, err := store.ListThreads(ctx, "tenant", "parent")
	require.NoError(t, err)
	require.Len(t, threads, 1)
}
