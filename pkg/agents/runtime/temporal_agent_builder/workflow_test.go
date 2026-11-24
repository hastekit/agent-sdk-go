package temporal_agent_builder_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentconfig"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_agent_builder"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// builderEnv registers the AgentBuilder workflow and its activities, as a
// worker does at start — before any config exists.
func builderEnv(t *testing.T, f *testutil.AgentFixture, broker agents.StreamBroker) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	temporal_agent_builder.New(f.Builder, broker).Register(env)
	return env
}

func builderInput(namespace string) *agents.AgentInput {
	return &agents.AgentInput{
		Namespace: namespace,
		ThreadID:  "thread-1",
		StreamID:  "stream-1",
		Message:   history.Message{ID: "m1", Messages: []responses.InputMessageUnion{responses.UserMessage("go")}},
	}
}

// A config written after the worker registered runs durably, through the
// fixed-name activities, with only its picked tools, skills and MCP tools.
func TestAgentBuilderWorkflowRunsAConfigAddedAfterWorkerStart(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, nil, broker, testutil.FixtureSteps()...)
	env := builderEnv(t, f, broker)

	f.AddConfig(t)
	snapshot, err := f.Registry.Snapshot(context.Background(), testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)

	env.ExecuteWorkflow(temporal_agent_builder.WorkflowName, snapshot, builderInput(testutil.FixtureNamespace))
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out agents.AgentOutput
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, agentstate.RunStatusCompleted, out.Status)

	seen := f.Provider.Seen()
	require.Len(t, seen, 4)
	assert.Equal(t, []string{testutil.FixturePickedMCP, testutil.FixturePickedTool, agents.ReadSkillToolName}, seen[0].Tools)
	assert.Contains(t, seen[0].Instructions, testutil.FixturePickedSkill)
	assert.NotContains(t, seen[0].Instructions, testutil.FixtureOtherSkill)
	assert.Equal(t, 1, f.Calls.Of(testutil.FixturePickedTool))
	assert.Equal(t, 1, f.Calls.Of(testutil.FixturePickedMCP))
	assert.Zero(t, f.Calls.Of(testutil.FixtureOtherTool))
	assert.Zero(t, f.Calls.Of(testutil.FixtureOtherMCPTool))
}

// A snapshot resolved for one namespace never runs against another's data.
func TestAgentBuilderWorkflowRefusesAnotherNamespacesSnapshot(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, nil, broker, testutil.FixtureSteps()...)
	env := builderEnv(t, f, broker)
	f.AddConfig(t)
	snapshot, err := f.Registry.Snapshot(context.Background(), testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)

	env.ExecuteWorkflow(temporal_agent_builder.WorkflowName, snapshot, builderInput("tenant-b"))
	err = env.GetWorkflowError()
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, temporal_agent_builder.ConfigInvalidErrorType, appErr.Type())
	assert.True(t, appErr.NonRetryable())
	assert.Empty(t, f.Provider.Seen(), "nothing ran")
}

// A model the worker does not allowlist fails once, not on every retry.
func TestAgentBuilderModelActivityFailsPermanentlyOnAnUnknownModel(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, nil, broker)
	env := builderEnv(t, f, broker)
	snapshot := &agentconfig.Snapshot{Namespace: testutil.FixtureNamespace, Root: "rogue", Agents: map[string]agentconfig.Config{
		"rogue": {Name: "rogue", Model: "OpenAI/not-allowlisted"},
	}}

	attempts := 0
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "AgentBuilder_LLMCallActivity" {
			attempts++
		}
	})
	env.ExecuteWorkflow(temporal_agent_builder.WorkflowName, snapshot, builderInput(testutil.FixtureNamespace))
	require.Error(t, env.GetWorkflowError())
	assert.Equal(t, 1, attempts, "a configuration error must not be retried")
}

// A tool the worker's catalog lacks fails the workflow before any activity.
func TestAgentBuilderWorkflowRefusesAToolOutsideTheCatalog(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, nil, broker)
	env := builderEnv(t, f, broker)
	snapshot := &agentconfig.Snapshot{Namespace: testutil.FixtureNamespace, Root: "rogue", Agents: map[string]agentconfig.Config{
		"rogue": {Name: "rogue", Model: testutil.FixtureModel, Tools: []string{"rm_rf"}},
	}}
	env.ExecuteWorkflow(temporal_agent_builder.WorkflowName, snapshot, builderInput(testutil.FixtureNamespace))
	var appErr *temporal.ApplicationError
	require.True(t, errors.As(env.GetWorkflowError(), &appErr))
	assert.Equal(t, temporal_agent_builder.ConfigInvalidErrorType, appErr.Type())
}

// The builder binds a durable runtime through the plain AgentOptions.Runtime
// field: the agent it builds starts the generic workflow, not its own.
func TestRuntimeIsAnAgentConfigRuntime(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	runtime := temporal_agent_builder.NewRuntime(nil, broker)
	f := testutil.NewAgentFixture(t, runtime, nil)
	f.AddConfig(t)
	agent, err := f.Agent(context.Background(), testutil.FixtureNamespace, testutil.FixtureAgent)
	require.NoError(t, err)
	require.Same(t, broker, agent.StreamBroker(), "the agent streams on the runtime's broker")
	_, err = agent.Run(context.Background(), builderInput(testutil.FixtureNamespace))
	require.ErrorContains(t, err, "no temporal client", "the run went to the runtime, not in process")
	assert.Empty(t, f.Provider.Seen())
}

func backgroundSnapshot(t *testing.T, f *testutil.AgentFixture) *agentconfig.Snapshot {
	t.Helper()
	_, err := f.Store.Put(context.Background(), testutil.FixtureNamespace, agentconfig.Config{Name: "indexer", Model: testutil.FixtureModel, Tools: []string{testutil.FixtureBackgroundTool}})
	require.NoError(t, err)
	snapshot, err := f.Registry.Snapshot(context.Background(), testutil.FixtureNamespace, "indexer")
	require.NoError(t, err)
	return snapshot
}

// A catalog tool's task gets a workflow of its own, which carries the
// snapshot and the tool's catalog key so it can wait and deliver without the
// run that started it.
func TestBackgroundToolStartsItsOwnWorkflow(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, nil, broker,
		testutil.Step{Tool: testutil.FixtureBackgroundTool},
		testutil.Step{Text: "indexing has started"},
	)
	env := builderEnv(t, f, broker)
	snapshot := backgroundSnapshot(t, f)

	var started *temporal_agent_builder.BackgroundTaskInput
	env.OnWorkflow("AgentBuilder_BackgroundTaskWorkflow", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		started, _ = args.Get(1).(*temporal_agent_builder.BackgroundTaskInput)
	})
	env.ExecuteWorkflow(temporal_agent_builder.WorkflowName, snapshot, builderInput(testutil.FixtureNamespace))
	require.NoError(t, env.GetWorkflowError())
	var out agents.AgentOutput
	require.NoError(t, env.GetWorkflowResult(&out))
	require.Equal(t, agentstate.RunStatusCompleted, out.Status)

	require.NotNil(t, started, "the task's workflow was started")
	assert.Equal(t, testutil.FixtureBackgroundTool, started.Tool)
	assert.Equal(t, "indexer", started.Agent)
	assert.Equal(t, "indexer", started.Snapshot.Root)
	assert.Equal(t, "task-1", started.Ref.TaskID)
	assert.Equal(t, testutil.FixtureNamespace, started.Ref.Namespace)
	assert.Zero(t, f.Calls.Of(testutil.FixtureBackgroundTool+":await"), "the run itself does not wait")
}

// The task's workflow waits on the catalog tool, then — the thread being idle —
// starts the snapshot's agent with the result.
func TestBackgroundWorkflowDeliversIntoANewRunOfTheSnapshot(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, nil, broker)
	env := builderEnv(t, f, broker)
	snapshot := backgroundSnapshot(t, f)

	var delivered *agents.AgentInput
	var rerun *agentconfig.Snapshot
	env.OnWorkflow(temporal_agent_builder.WorkflowName, mock.Anything, mock.Anything, mock.Anything).Return(&agents.AgentOutput{}, nil).Run(func(args mock.Arguments) {
		rerun, _ = args.Get(1).(*agentconfig.Snapshot)
		delivered, _ = args.Get(2).(*agents.AgentInput)
	})
	ref := agents.BackgroundTaskRef{
		TaskID: "task-1", CallID: "call-1", ToolName: testutil.FixtureBackgroundTool, AgentName: "indexer",
		Namespace: testutil.FixtureNamespace, ThreadID: "thread-bg", TaskStreamID: "task-stream", ThreadStreamID: "thread-stream",
	}
	env.ExecuteWorkflow("AgentBuilder_BackgroundTaskWorkflow", &temporal_agent_builder.BackgroundTaskInput{
		Snapshot: snapshot, Agent: "indexer", Tool: testutil.FixtureBackgroundTool, Ref: ref,
	})
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, 1, f.Calls.Of(testutil.FixtureBackgroundTool+":await"))

	require.NotNil(t, delivered, "an idle thread gets a new run")
	require.NotNil(t, rerun)
	assert.Equal(t, "indexer", rerun.Root)
	assert.Equal(t, testutil.FixtureNamespace, delivered.Namespace)
	assert.Equal(t, "thread-stream", delivered.StreamID)
	message, err := json.Marshal(delivered.Message)
	require.NoError(t, err)
	assert.Contains(t, string(message), testutil.FixtureBackgroundResult)
}

// A sub-agent call leaves the workflow for an activity carrying the snapshot,
// which builds and runs the sub-agent; its answer is the tool's result. With
// no runtime the fixture runs it in the activity's process.
func TestSubAgentCallRunsInAnActivity(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	f := testutil.NewAgentFixture(t, nil, broker,
		testutil.Step{Tool: testutil.FixtureAgent, Arguments: `{"message":"look it up"}`},
		testutil.Step{Text: "it is 42"},
		testutil.Step{Text: "the answer is 42"},
	)
	env := builderEnv(t, f, broker)
	f.AddConfig(t)
	_, err := f.Store.Put(context.Background(), testutil.FixtureNamespace, agentconfig.Config{
		Name: "orchestrator", Model: testutil.FixtureModel, SubAgents: []agentconfig.SubAgent{{Name: testutil.FixtureAgent}},
	})
	require.NoError(t, err)
	snapshot, err := f.Registry.Snapshot(context.Background(), testutil.FixtureNamespace, "orchestrator")
	require.NoError(t, err)

	calls := 0
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "AgentBuilder_SubAgentCallActivity" {
			calls++
		}
	})
	env.ExecuteWorkflow(temporal_agent_builder.WorkflowName, snapshot, builderInput(testutil.FixtureNamespace))
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, 1, calls)

	seen := f.Provider.Seen()
	require.Len(t, seen, 3)
	assert.Equal(t, []string{testutil.FixtureAgent}, seen[0].Tools)
	assert.Contains(t, strings.Join(seen[1].Texts, "\n"), "look it up")
	assert.Contains(t, strings.Join(seen[2].Texts, "\n"), "it is 42")
}
