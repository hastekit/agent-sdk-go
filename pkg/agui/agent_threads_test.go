package agui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// threadIDs lists the threads GET /agents/{agent}/threads returns.
func threadIDs(t *testing.T, server *httptest.Server, agent string) []string {
	t.Helper()
	res, err := http.Get(server.URL + "/agents/" + agent + "/threads")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	var listing struct {
		Threads []history.ThreadInfo `json:"threads"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&listing))
	ids := []string{}
	for _, thread := range listing.Threads {
		ids = append(ids, thread.ThreadID)
	}
	return ids
}

// Agents sharing one history store each list only the threads they were
// started with: a handoff does not move a thread to the specialist, and a
// thread saved before runs recorded their agent goes by its first reply's.
func TestAgentsSharingAStoreListOnlyTheirOwnThreads(t *testing.T) {
	persistence := history.NewInMemoryConversationPersistence()
	shared := history.NewConversationManager(persistence)
	provider := testutil.NewScriptedProvider(
		testutil.Step{Text: "a joke"},
		testutil.Step{Tool: "transfer_to_agent", Arguments: `{"agent_name":"specialist"}`},
		testutil.Step{Text: "the specialist's answer"},
		testutil.Step{Text: "the specialist's follow-up"},
	)
	joker := agents.NewAgent(&agents.AgentOptions{Name: "joker", LLM: provider, History: shared})
	specialist := agents.NewAgent(&agents.AgentOptions{Name: "specialist", LLM: provider, History: shared})
	root := agents.NewAgent(&agents.AgentOptions{Name: "root", LLM: provider, History: shared, StickyHandoff: true,
		Handoffs: []*agents.Handoff{agents.NewHandoff("specialist", "Answers hard questions.", specialist)}})
	server := httptest.NewServer(NewHandler(registry{"joker": joker, "root": root, "specialist": specialist}))
	defer server.Close()

	postRun(t, server, "joker", RunAgentInput{ThreadID: "joke-thread", Messages: []Message{{ID: "u1", Role: RoleUser, Content: "tell a joke"}}})
	postRun(t, server, "root", RunAgentInput{ThreadID: "root-thread", Messages: []Message{{ID: "u2", Role: RoleUser, Content: "a hard question"}}})
	require.Len(t, provider.Seen(), 3, "root handed off, and the specialist answered in the same run")
	postRun(t, server, "root", RunAgentInput{ThreadID: "root-thread", Messages: []Message{{ID: "u3", Role: RoleUser, Content: "and then?"}}})
	require.Len(t, provider.Seen(), 4, "the next turn resumed straight in the specialist")

	// Written before runs recorded their agent: one answered by joker, one
	// that never got an answer and so belongs to no agent anyone can tell.
	ctx := context.Background()
	question := messages.New("user", []responses.InputMessageUnion{responses.UserMessage("an old question")})
	answer := messages.New("joker", []responses.InputMessageUnion{{OfOutputMessage: &responses.OutputMessage{
		ID: "msg_old", Role: constants.RoleAssistant,
		Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: "an old answer"}}},
	}}})
	require.NoError(t, persistence.SaveMessages(ctx, "default", "", "old-run", "", "old-thread", "old-conversation", []history.Message{question, answer}, map[string]any{}))
	require.NoError(t, persistence.SaveMessages(ctx, "default", "", "orphan-run", "", "orphan-thread", "orphan-conversation", []history.Message{question}, map[string]any{}))

	assert.ElementsMatch(t, []string{"joke-thread", "old-thread", "orphan-thread"}, threadIDs(t, server, "joker"))
	assert.ElementsMatch(t, []string{"root-thread", "orphan-thread"}, threadIDs(t, server, "root"))
	assert.ElementsMatch(t, []string{"orphan-thread"}, threadIDs(t, server, "specialist"), "a handoff, sticky or not, leaves the thread with the agent it entered at")

	threads, err := persistence.ListThreads(ctx, "default", "")
	require.NoError(t, err)
	agentOf := map[string]string{}
	for _, thread := range threads {
		agentOf[thread.ThreadID] = thread.AgentName
	}
	assert.Equal(t, map[string]string{"joke-thread": "joker", "root-thread": "root", "old-thread": "joker", "orphan-thread": ""}, agentOf)
}
