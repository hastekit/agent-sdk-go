package history

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

// A thread belongs to the agent its first run recorded. One saved before runs
// recorded their agent goes by its first reply's sender, which a file store
// reads back in a different shape than it was written, or by the agent whose
// loop ran its failed first run; one with neither goes by nobody.
func TestThreadsKnowTheirAgentAcrossAReopenedFileStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)

	question := messages.New("alice", []responses.InputMessageUnion{responses.UserMessage("a question")})
	answer := func(agent string) Message {
		return messages.New(agent, []responses.InputMessageUnion{{OfOutputMessage: &responses.OutputMessage{
			ID: "msg_" + agent, Role: constants.RoleAssistant,
			Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: "an answer"}}},
		}}})
	}
	// Recorded: the run entered at triage, and a specialist answered.
	require.NoError(t, store.SaveMessages(ctx, "ns", "", "run-1", "", "recorded", "c1", []Message{question, answer("specialist")}, map[string]any{AgentMetaKey: "triage"}))
	require.NoError(t, store.SaveMessages(ctx, "ns", "", "run-2", "run-1", "recorded", "c1", []Message{question, answer("specialist")}, map[string]any{AgentMetaKey: "triage"}))
	// Saved before runs recorded their agent.
	require.NoError(t, store.SaveMessages(ctx, "ns", "", "run-3", "", "legacy", "c2", []Message{question, answer("helper")}, map[string]any{}))
	require.NoError(t, store.SaveMessages(ctx, "ns", "", "run-4", "", "unanswered", "c3", []Message{question}, map[string]any{}))
	require.NoError(t, store.SaveMessages(ctx, "ns", "", "run-5", "", "failed", "c4", []Message{question}, map[string]any{"run_state": map[string]any{"status": "error", "last_agent_name": "helper"}}))
	require.NoError(t, store.Close())

	reopened, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	defer reopened.Close()
	threads, err := reopened.ListThreads(ctx, "ns", "")
	require.NoError(t, err)
	agentOf := map[string]string{}
	for _, thread := range threads {
		agentOf[thread.ThreadID] = thread.AgentName
	}
	require.Equal(t, map[string]string{"recorded": "triage", "legacy": "helper", "failed": "helper", "unanswered": ""}, agentOf)
}
