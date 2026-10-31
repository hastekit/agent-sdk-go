package subagents

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type mapRegistry map[string]*agents.Agent

func (r mapRegistry) Agent(name string) (*agents.Agent, bool) {
	agent, ok := r[name]
	return agent, ok
}

func (r mapRegistry) AgentNames() []string { return slices.Collect(maps.Keys(r)) }

// textLLM answers every call with text.
type textLLM string

func (l textLLM) NewStreamingResponses(context.Context, *agents.ModelCall, *responses.Request, func(*responses.ResponseChunk)) (*responses.Response, error) {
	return &responses.Response{Output: []responses.OutputMessageUnion{{OfOutputMessage: &responses.OutputMessage{
		ID: responses.NewOutputItemMessageID(), Role: constants.RoleAssistant,
		Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: string(l)}}},
	}}}}, nil
}

func newAgent(name, description, answer string, store history.ConversationPersistenceAdapter) *agents.Agent {
	return agents.NewAgent(&agents.AgentOptions{Name: name, Description: description, History: history.NewConversationManager(store)}).WithLLM(textLLM(answer))
}

func names(listed []agents.SubAgentInfo) []string {
	var out []string
	for _, info := range listed {
		out = append(out, info.Name)
	}
	return out
}

// A client lists the registry's agents but the caller, by name, narrowed by
// its options; WithSelf lists the caller first, as itself.
func TestClientListsTheRegistrysAgents(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	registry := mapRegistry{}
	query := agents.SubAgentQuery{Caller: "lead", Namespace: "tenant"}

	// Made before the agents it serves, which it finds once registered.
	client := NewClient(registry)
	listed, err := client.ListSubAgents(ctx, query)
	require.NoError(t, err)
	require.Empty(t, listed)

	registry["writer"] = newAgent("writer", "Writes things.", "", store)
	registry["researcher"] = newAgent("researcher", "Finds sources.", "", store)
	registry["lead"] = newAgent("lead", "Leads.", "", store)

	listed, err = client.ListSubAgents(ctx, query)
	require.NoError(t, err)
	require.Equal(t, []agents.SubAgentInfo{{Name: "researcher", Description: "Finds sources."}, {Name: "writer", Description: "Writes things."}}, listed)

	listed, err = NewClient(registry, WithSelf()).ListSubAgents(ctx, query)
	require.NoError(t, err)
	require.Equal(t, agents.SubAgentInfo{Name: "lead", Description: "Leads.", Self: true}, listed[0])
	require.Equal(t, []string{"lead", "researcher", "writer"}, names(listed))

	listed, err = NewClient(registry, WithAgents("writer", "lead")).ListSubAgents(ctx, query)
	require.NoError(t, err)
	require.Equal(t, []string{"writer"}, names(listed), "the caller is listed only as itself")

	listed, err = NewClient(registry, WithoutAgents("writer")).ListSubAgents(ctx, query)
	require.NoError(t, err)
	require.Equal(t, []string{"researcher"}, names(listed))

	// A filter sees who is asking, for which run, and the caller's own entry.
	onlyForPro := WithFilter(func(_ context.Context, query agents.SubAgentQuery, info agents.SubAgentInfo) bool {
		return info.Name != "researcher" || query.RunContext["plan"] == "pro"
	})
	noSelfForFree := WithFilter(func(_ context.Context, query agents.SubAgentQuery, info agents.SubAgentInfo) bool {
		return !info.Self || query.RunContext["plan"] == "pro"
	})
	filtered := NewClient(registry, WithSelf(), onlyForPro, noSelfForFree)
	listed, err = filtered.ListSubAgents(ctx, agents.SubAgentQuery{Caller: "lead", RunContext: map[string]any{"plan": "free"}})
	require.NoError(t, err)
	require.Equal(t, []string{"writer"}, names(listed))
	listed, err = filtered.ListSubAgents(ctx, agents.SubAgentQuery{Caller: "lead", RunContext: map[string]any{"plan": "pro"}})
	require.NoError(t, err)
	require.Equal(t, []string{"lead", "researcher", "writer"}, names(listed))
}

// A message goes to the agent its name resolves to — the caller itself for its
// own entry — and only to one the client would list for the caller's run.
func TestClientRunsOnlyTheAgentsItLists(t *testing.T) {
	ctx := context.Background()
	store := history.NewInMemoryConversationPersistence()
	lead := newAgent("lead", "", "the lead's copy answers", store)
	registry := mapRegistry{
		"lead":   newAgent("lead", "", "the registered lead answers", store),
		"writer": newAgent("writer", "", "the writer answers", store),
		"critic": newAgent("critic", "", "the critic answers", store),
	}
	client := NewClient(registry, WithSelf(), WithoutAgents("critic"))

	request := func(name, threadID string) agents.SubAgentRequest {
		return agents.SubAgentRequest{Caller: lead, Name: name, Input: &agents.AgentInput{
			Namespace: "tenant", ThreadID: threadID, ParentThreadID: "parent", Hidden: true,
			StreamID: agents.StreamIDForThread("tenant", threadID),
			Message:  messages.NewWithID("m_"+threadID, "lead", []responses.InputMessageUnion{responses.UserMessage("go")}),
		}}
	}

	outcome, err := client.RunSubAgent(ctx, request("writer", "t-writer"))
	require.NoError(t, err)
	require.Equal(t, "the writer answers", outcome.Output.Text())

	outcome, err = client.RunSubAgent(ctx, request("lead", "t-self"))
	require.NoError(t, err)
	require.Equal(t, "the lead's copy answers", outcome.Output.Text(), "a copy of the caller as it is, not the agent registered under its name")

	_, err = client.RunSubAgent(ctx, request("critic", "t-critic"))
	require.ErrorContains(t, err, `lead may not call sub-agent "critic"`)
	_, err = NewClient(registry).RunSubAgent(ctx, request("lead", "t-self-denied"))
	require.ErrorContains(t, err, `lead may not call sub-agent "lead"`)

	steered, err := client.SteerSubAgent(ctx, request("writer", "t-writer"))
	require.NoError(t, err)
	require.False(t, steered, "no run is going on the thread")
	_, err = client.SteerSubAgent(ctx, request("critic", "t-critic"))
	require.Error(t, err)
}
