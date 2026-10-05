package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/tools"
	"github.com/stretchr/testify/require"
)

func TestAgentToolKeepsSubAgentThreadsInCallerNamespace(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	sub := newTestAgent(t, "researcher", "found it", store)
	tool := tools.NewAgentTool("research", "Research a topic", sub, tools.SubAgentContextModeNone, tools.WithoutTracing)

	// The caller's own thread, in the default group, owns the shared session.
	require.NoError(t, store.SaveMessages(ctx, "tenant", "", "", false, "parent-run", "", "parent", "session", nil, nil))

	call := toolCall("research", `{"message":"look this up"}`, "parent")
	call.SessionID = "session"
	resp, err := tool.Execute(ctx, call)
	require.NoError(t, err)
	output := *resp.Output.OfString
	require.True(t, strings.HasPrefix(output, "found it"))
	threadID := strings.TrimSpace(output[strings.LastIndex(output, "Thread ID:")+len("Thread ID:"):])

	rows, err := history.LoadTranscript(ctx, store, "tenant", threadID)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "sub-agent thread is stored in the caller's namespace")
	require.Equal(t, "parent", rows[0].GroupID, "grouped under the origin thread, not the shared session's group")
	require.Equal(t, "parent", rows[0].ParentThreadID)
	require.True(t, rows[0].Hidden)
}

// A sub-agent's thread goes into the group the call names for new threads —
// the calling thread, as the agent loop sets it — whether the call waits for
// the answer or not; parent_thread_id is always the calling thread.
func TestAgentToolsGroupSubAgentThreadsByTheCallsGroupID(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	sub := newTestAgent(t, "researcher", "found it", store)

	sync := tools.NewAgentTool("research", "Research a topic", sub, tools.SubAgentContextModeNone)
	call := toolCall("research", `{"message":"look this up"}`, "parent")
	call.GroupID = "group-from-call"
	resp, err := sync.Execute(ctx, call)
	require.NoError(t, err)
	syncThread := threadIDFrom(t, *resp.Output.OfString)

	async := tools.NewAsyncAgentTool("research", "Research a topic", sub, tools.SubAgentContextModeNone)
	asyncCall := toolCall("research", `{"message":"and this"}`, "parent")
	asyncCall.CallID = "call_async"
	asyncCall.GroupID = "group-from-call"
	started, err := async.Execute(ctx, asyncCall)
	require.NoError(t, err)
	asyncThread := threadIDFrom(t, awaitTask(t, async, started, asyncCall))

	for _, threadID := range []string{syncThread, asyncThread} {
		rows, err := history.LoadTranscript(ctx, store, "tenant", threadID)
		require.NoError(t, err)
		require.NotEmpty(t, rows)
		require.Equal(t, "group-from-call", rows[0].GroupID)
		require.Equal(t, "parent", rows[0].ParentThreadID)
	}
}
