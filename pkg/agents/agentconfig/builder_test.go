package agentconfig_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func userTurn(text string) history.Message {
	return history.Message{Messages: []responses.InputMessageUnion{responses.UserMessage(text)}}
}

// AssertOnlyPicks checks that the model was offered exactly the config's
// picks, and that each one ran, while nothing it did not pick ever did.
func assertOnlyPicks(t *testing.T, f *testutil.AgentFixture) {
	t.Helper()
	seen := f.Provider.Seen()
	require.NotEmpty(t, seen)
	assert.Equal(t, []string{testutil.FixturePickedMCP, testutil.FixturePickedTool, agents.ReadSkillToolName}, seen[0].Tools,
		"the model must be offered the picked tool, MCP tool and skill reader, and nothing else")
	assert.Contains(t, seen[0].Instructions, testutil.FixturePickedSkill)
	assert.NotContains(t, seen[0].Instructions, testutil.FixtureOtherSkill, "an unpicked skill must not be listed")
	assert.Equal(t, 1, f.Calls.Of(testutil.FixturePickedTool))
	assert.Equal(t, 1, f.Calls.Of(testutil.FixturePickedMCP))
	assert.Zero(t, f.Calls.Of(testutil.FixtureOtherTool))
	assert.Zero(t, f.Calls.Of(testutil.FixtureOtherMCPTool))
}

// A config written after the Builder exists runs locally with its picks only.
func TestBuilderRunsAConfigAddedAtRunTime(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil, testutil.FixtureSteps()...)
	ctx := context.Background()

	_, err := f.Agent(ctx, testutil.FixtureNamespace, testutil.FixtureAgent)
	require.ErrorIs(t, err, agentconfig.ErrNotFound, "nothing is stored yet")

	f.AddConfig(t)
	agent, err := f.Agent(ctx, testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)
	out, err := agent.Run(ctx, &agents.AgentInput{Namespace: testutil.FixtureNamespace, Message: userTurn("go")})
	require.NoError(t, err)
	require.Equal(t, agentstate.RunStatusCompleted, out.Status)
	assertOnlyPicks(t, f)

	// The skill reader read the picked skill's instructions.
	require.Len(t, f.Provider.Seen(), 4)
}

func TestBuilderNeverFindsAnotherNamespacesAgent(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil)
	f.AddConfig(t)
	for _, namespace := range []string{"tenant-b", ""} {
		_, err := f.Agent(context.Background(), namespace, testutil.FixtureAgent)
		require.ErrorIs(t, err, agentconfig.ErrNotFound, "namespace %q", namespace)
	}
}

func TestBuilderValidateRefusesWhatTheServerDoesNotOffer(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil)
	require.NoError(t, f.Builder.Validate(f.Config))

	unknownModel := f.Config
	unknownModel.Model = "OpenAI/not-allowlisted"
	require.ErrorIs(t, f.Builder.Validate(unknownModel), agentconfig.ErrInvalid)

	unknownTool := f.Config
	unknownTool.Tools = []string{"rm_rf"}
	require.ErrorIs(t, f.Builder.Validate(unknownTool), agentconfig.ErrInvalid)

	// Skill and MCP picks name catalogs that change at run time, so an absent
	// pick is allowed and simply ignored.
	absentPicks := f.Config
	absentPicks.Skills = []string{"not-yet-uploaded"}
	require.NoError(t, f.Builder.Validate(absentPicks))
}

// Nothing is cached between lookups, so an edit takes effect on the next one.
func TestRegistryBuildsTheConfigAsStoredNow(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil, testutil.Step{Text: "done"})
	f.AddConfig(t)
	ctx := context.Background()
	_, err := f.Agent(ctx, testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)

	edited := f.Config
	edited.Instruction = "You research questions carefully."
	_, err = f.Store.Put(ctx, testutil.FixtureNamespace, edited)
	require.NoError(t, err)
	agent, err := f.Agent(ctx, testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)
	out, err := agent.Run(ctx, &agents.AgentInput{Namespace: testutil.FixtureNamespace, Message: userTurn("go")})
	require.NoError(t, err)
	require.Equal(t, agentstate.RunStatusCompleted, out.Status)
	require.NotEmpty(t, f.Provider.Seen())
	assert.Contains(t, f.Provider.Seen()[0].Instructions, "carefully")
}

func TestBuilderWiresHandoffsWithinTheSnapshot(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil,
		testutil.Step{Tool: "transfer_to_agent", Arguments: `{"agent_name":"specialist"}`},
		testutil.Step{Text: "specialist answer"},
	)
	ctx := context.Background()
	specialist := agentconfig.Config{Name: "specialist", Model: testutil.FixtureModel, Handoffs: []agentconfig.Handoff{{Name: "triage"}}}
	triage := agentconfig.Config{Name: "triage", Model: testutil.FixtureModel, Handoffs: []agentconfig.Handoff{{Name: "specialist", Description: "answers"}, {Name: "missing"}}}
	for _, cfg := range []agentconfig.Config{specialist, triage} {
		_, err := f.Store.Put(ctx, testutil.FixtureNamespace, cfg)
		require.NoError(t, err)
	}

	snapshot, err := f.Registry.Snapshot(ctx, testutil.FixtureNamespace, "triage")
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 2, "a missing target is left out")
	require.Len(t, snapshot.Agents["triage"].Handoffs, 1, "and so is its edge")

	agent, err := f.Builder.Build(snapshot)
	require.NoError(t, err)
	out, err := agent.Run(ctx, &agents.AgentInput{Namespace: testutil.FixtureNamespace, Message: userTurn("help")})
	require.NoError(t, err)
	require.Equal(t, agentstate.RunStatusCompleted, out.Status)
	require.Len(t, f.Provider.Seen(), 2, "the specialist answered inside the same run")
}

// countingModels counts resolutions, to show a lookup built nothing.
type countingModels struct {
	inner    agentconfig.ModelResolver
	resolved int
}

func (m *countingModels) ResolveModel(id string) (llm.Provider, error) {
	m.resolved++
	return m.inner.ResolveModel(id)
}

func TestAllowModelsBindsEachAllowedIdOnce(t *testing.T) {
	bound := 0
	models := agentconfig.AllowModels(func(id string) (llm.Provider, error) {
		bound++
		return testutil.NewScriptedProvider(), nil
	}, "Test/a")
	first, err := models.ResolveModel("Test/a")
	require.NoError(t, err)
	second, err := models.ResolveModel("Test/a")
	require.NoError(t, err)
	require.Same(t, first, second)
	require.Equal(t, 1, bound)

	_, err = models.ResolveModel("Test/b")
	require.ErrorIs(t, err, agentconfig.ErrInvalid)
	require.Equal(t, 1, bound, "a refused id is never bound")
}

func TestNewBuilderRequiresModels(t *testing.T) {
	_, err := agentconfig.NewBuilder(agentconfig.Dependencies{})
	require.Error(t, err)
}

func TestSnapshotCheckRefusesAnotherNamespace(t *testing.T) {
	snapshot := &agentconfig.Snapshot{Namespace: "tenant-a", Root: "a", Agents: map[string]agentconfig.Config{"a": {Name: "a", Model: "m"}}}
	require.NoError(t, snapshot.Check("tenant-a"))
	err := snapshot.Check("tenant-b")
	require.ErrorIs(t, err, agentconfig.ErrInvalid)
	require.True(t, strings.Contains(err.Error(), "tenant-b"))

	var nilSnapshot *agentconfig.Snapshot
	require.True(t, errors.Is(nilSnapshot.Check("tenant-a"), agentconfig.ErrInvalid))
}

// A config's catalog tool that starts a background task runs locally: the
// task is waited on in process and its result starts a run of the same agent.
func TestBuilderRunsBackgroundToolsLocally(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil,
		testutil.Step{Tool: testutil.FixtureBackgroundTool},
		testutil.Step{Text: "indexing has started"},
		testutil.Step{Text: "indexing finished"},
	)
	ctx := context.Background()
	_, err := f.Store.Put(ctx, testutil.FixtureNamespace, agentconfig.Config{Name: "indexer", Model: testutil.FixtureModel, Tools: []string{testutil.FixtureBackgroundTool}})
	require.NoError(t, err)
	agent, err := f.Agent(ctx, testutil.FixtureNamespace, "indexer")
	require.NoError(t, err)

	out, err := agent.Run(ctx, &agents.AgentInput{Namespace: testutil.FixtureNamespace, ThreadID: "thread-bg", Message: userTurn("index the docs")})
	require.NoError(t, err)
	require.Equal(t, agentstate.RunStatusCompleted, out.Status)
	agent.WaitForBackgroundTasks()

	seen := f.Provider.Seen()
	require.Len(t, seen, 3, "the task's result starts a run of its own")
	assert.Contains(t, strings.Join(seen[2].Texts, "\n"), testutil.FixtureBackgroundResult)
	assert.Equal(t, 1, f.Calls.Of(testutil.FixtureBackgroundTool+":await"))
}

// A config calls another as a tool: the model is offered the sub-agent by
// name, the sub-agent answers in the caller's namespace — with its own picks —
// in a conversation kept apart from the namespace's chats, and the answer
// comes back as the tool's result. Isolated, the default, keeps one such
// conversation per thread of the caller's.
func TestSubAgentsAreToolsThatRunInTheCallersNamespace(t *testing.T) {
	f := testutil.NewAgentFixture(t, nil, nil,
		testutil.Step{Tool: testutil.FixtureAgent, Arguments: `{"message":"look it up"}`},
		testutil.Step{Text: "it is 42"},
		testutil.Step{Text: "the answer is 42"},
		testutil.Step{Tool: testutil.FixtureAgent, Arguments: `{"message":"and again"}`},
		testutil.Step{Text: "still 42"},
		testutil.Step{Text: "still 42, it says"},
	)
	f.AddConfig(t)
	ctx := context.Background()
	_, err := f.Store.Put(ctx, testutil.FixtureNamespace, agentconfig.Config{
		Name: "orchestrator", Model: testutil.FixtureModel,
		SubAgents: []agentconfig.SubAgent{{Name: testutil.FixtureAgent}, {Name: "missing"}},
	})
	require.NoError(t, err)

	snapshot, err := f.Registry.Snapshot(ctx, testutil.FixtureNamespace, "orchestrator")
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 2, "the sub-agent is part of the run's graph; a missing one is not")
	require.Len(t, snapshot.Agents["orchestrator"].SubAgents, 1)

	agent, err := f.Builder.Build(snapshot)
	require.NoError(t, err)
	for _, turn := range []string{"what is it?", "check again"} {
		out, err := agent.Run(ctx, &agents.AgentInput{Namespace: testutil.FixtureNamespace, ThreadID: "main-thread", Message: userTurn(turn)})
		require.NoError(t, err)
		require.Equal(t, agentstate.RunStatusCompleted, out.Status)
	}

	seen := f.Provider.Seen()
	require.Len(t, seen, 6)
	assert.Equal(t, []string{testutil.FixtureAgent}, seen[0].Tools, "offered as a tool named after it")
	assert.Contains(t, strings.Join(seen[1].Texts, "\n"), "look it up")
	assert.Equal(t, []string{testutil.FixturePickedMCP, testutil.FixturePickedTool, agents.ReadSkillToolName}, seen[1].Tools,
		"the sub-agent has its own picks, the namespace's included")
	assert.Contains(t, strings.Join(seen[2].Texts, "\n"), "it is 42", "its answer is the tool's result")
	assert.Contains(t, strings.Join(seen[4].Texts, "\n"), "look it up", "isolated: the second call continues the first conversation")
	assert.NotContains(t, strings.Join(seen[1].Texts, "\n"), "what is it?", "the sub-agent does not see the caller's conversation")

	lister := f.Builder.History().ConversationPersistenceAdapter.(history.ThreadLister)
	chats, err := lister.ListThreads(ctx, testutil.FixtureNamespace, history.DefaultGroupID)
	require.NoError(t, err)
	require.Len(t, chats, 1)
	assert.Equal(t, "main-thread", chats[0].ThreadID)
	assert.Equal(t, "orchestrator", chats[0].AgentName)
	subs, err := lister.ListThreads(ctx, testutil.FixtureNamespace, agentconfig.SubAgentGroupID)
	require.NoError(t, err)
	require.Len(t, subs, 1, "one conversation with the sub-agent for the caller's thread")
	assert.Equal(t, testutil.FixtureAgent, subs[0].AgentName)
}

func TestSubAgentsAreValidated(t *testing.T) {
	model := testutil.FixtureModel
	for name, cfg := range map[string]agentconfig.Config{
		"itself":                  {Name: "a", Model: model, SubAgents: []agentconfig.SubAgent{{Name: "a"}}},
		"twice":                   {Name: "a", Model: model, SubAgents: []agentconfig.SubAgent{{Name: "b"}, {Name: "b"}}},
		"an unknown context mode": {Name: "a", Model: model, SubAgents: []agentconfig.SubAgent{{Name: "b", ContextMode: "Shared"}}},
		"a picked tool's name":    {Name: "a", Model: model, Tools: []string{"lookup"}, SubAgents: []agentconfig.SubAgent{{Name: "lookup"}}},
		"two that name one tool":  {Name: "a", Model: model, SubAgents: []agentconfig.SubAgent{{Name: "b c"}, {Name: "b_c"}}},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, agentconfig.Validate(cfg), agentconfig.ErrInvalid)
		})
	}
	require.NoError(t, agentconfig.Validate(agentconfig.Config{Name: "a", Model: model, SubAgents: []agentconfig.SubAgent{{Name: "b", ContextMode: "None"}}}))
	assert.Equal(t, "research_assistant", agentconfig.SubAgent{Name: "research assistant"}.ToolName())
}
