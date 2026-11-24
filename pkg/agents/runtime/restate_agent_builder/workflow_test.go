package restate_agent_builder

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	restate "github.com/restatedev/sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The workflow wraps the Builder's real dependencies, which hold only the
// config's picks; the durable steps change how they run, not what they are.
func TestBuildAgentWrapsOnlyThePicks(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, nil, broker)
	f.AddConfig(t)
	ctx := context.Background()
	snapshot, err := f.Builder.Snapshot(ctx, testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)

	real, err := f.Builder.AgentOptions(snapshot.Agents[testutil.FixtureAgent])
	require.NoError(t, err)
	require.Len(t, real.Tools, 1)
	assert.Equal(t, testutil.FixturePickedTool, real.Tools[0].GetToolDescriptor().ToolUnion.OfFunction.Name)
	skills, err := real.SkillClient.ListSkills(ctx, testutil.FixtureNamespace, nil)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	assert.Equal(t, testutil.FixturePickedSkill, skills[0].Name)
	_, tools, err := real.MCPClient.ListTools(ctx, testutil.FixtureNamespace, nil)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, testutil.FixturePickedMCP, tools[0].GetToolDescriptor().ToolUnion.OfFunction.Name)

	// No restate context needed: the proxies only store it.
	input := &Input{Snapshot: snapshot, WorkflowInput: restate_runtime.WorkflowInput{Namespace: testutil.FixtureNamespace}}
	agent, err := New(f.Builder, broker).buildAgent(nil, input, "stream-1", snapshot.Root, map[string]*agents.Agent{})
	require.NoError(t, err)
	assert.Equal(t, testutil.FixtureAgent, agent.Name)
}

// A snapshot from another namespace, or one naming what the service does not
// offer, is terminal: Restate must not retry it.
func TestRunRefusesSnapshotsTerminally(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, nil, broker)
	f.AddConfig(t)
	snapshot, err := f.Builder.Snapshot(context.Background(), testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)
	builder := New(f.Builder, broker)

	_, err = builder.Run(nil, &Input{Snapshot: snapshot, WorkflowInput: restate_runtime.WorkflowInput{Namespace: "tenant-b"}})
	require.ErrorIs(t, err, agentconfig.ErrInvalid)
	assert.True(t, restate.IsTerminalError(err))

	rogue := &agentconfig.Snapshot{Namespace: testutil.FixtureNamespace, Root: "rogue", Agents: map[string]agentconfig.Config{
		"rogue": {Name: "rogue", Model: "OpenAI/not-allowlisted"},
	}}
	_, err = builder.Run(nil, &Input{Snapshot: rogue, WorkflowInput: restate_runtime.WorkflowInput{Namespace: testutil.FixtureNamespace, StreamID: "s"}})
	assert.True(t, restate.IsTerminalError(err))
	assert.True(t, errors.Is(err, agentconfig.ErrInvalid))
}

func TestRuntimeIsAnAgentConfigRuntime(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, NewRuntime("http://127.0.0.1:1", broker), nil)
	f.AddConfig(t)
	agent, err := f.Builder.Agent(context.Background(), testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)
	require.Same(t, broker, agent.StreamBroker())
	_, err = agent.Run(context.Background(), &agents.AgentInput{Namespace: testutil.FixtureNamespace})
	require.Error(t, err, "the run went to the Restate ingress, which is not there")
	assert.Empty(t, f.Provider.Seen(), "nothing ran in process")
}
