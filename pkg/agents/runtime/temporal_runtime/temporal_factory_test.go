package temporal_runtime_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history/summariser"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func factoryInput(text string) *agents.AgentInput {
	return &agents.AgentInput{Namespace: "ns", ThreadID: "thread-" + text, Message: messages.New("user", []responses.InputMessageUnion{{
		OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)}},
	}})}
}

func newHistory() *history.CommonConversationManager {
	return history.NewConversationManager(history.NewInMemoryConversationPersistence())
}

func newFactoryWorker(t *testing.T, factory *agents.AgentFactory) *temporal_runtime.FactoryAgentWorkflow {
	t.Helper()
	worker, err := temporal_runtime.NewFactoryAgentWorkflow(factory, streambroker.NewMemoryStreamBroker())
	require.NoError(t, err)
	return worker
}

// newFactoryEnv registers one factory worker's workflows and activities, as Serve does,
// leaving out the workflow named stub so a test can register its own.
func newFactoryEnv(suite *testsuite.WorkflowTestSuite, worker *temporal_runtime.FactoryAgentWorkflow, stub ...string) *testsuite.TestWorkflowEnvironment {
	env := suite.NewTestWorkflowEnvironment()
	for name, fn := range worker.GetActivities() {
		env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	for name, fn := range worker.GetWorkflows() {
		if !slices.Contains(stub, name) {
			env.RegisterWorkflowWithOptions(fn, workflow.RegisterOptions{Name: name})
		}
	}
	return env
}

func runSpec(t *testing.T, env *testsuite.TestWorkflowEnvironment, spec agents.AgentSpec, in *agents.AgentInput) *agents.AgentOutput {
	t.Helper()
	env.ExecuteWorkflow(temporal_runtime.FactoryAgentWorkflowName, &spec, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out agents.AgentOutput
	require.NoError(t, env.GetWorkflowResult(&out))
	return &out
}

func runSpecError(t *testing.T, env *testsuite.TestWorkflowEnvironment, spec agents.AgentSpec) error {
	t.Helper()
	env.ExecuteWorkflow(temporal_runtime.FactoryAgentWorkflowName, &spec, factoryInput("x"))
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	return env.GetWorkflowError()
}

// One worker, started before any spec exists, runs whatever spec a run starts with,
// and never asks the store for it: the spec is the workflow's input.
func TestFactoryWorkflow_RunsTheSpecItStartsWith(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	store := testutil.NewSpecStore()
	worker := newFactoryWorker(t, store.Factory(newHistory(), testutil.NewTextTool("lookup", "found"), testutil.NewTextTool("search", "searched")))

	out := runSpec(t, newFactoryEnv(&suite, worker), testutil.Spec("an3", "1", "v1 call:lookup", "lookup"), factoryInput("first"))
	assert.Equal(t, "v1 call:lookup | found", out.Text())

	out = runSpec(t, newFactoryEnv(&suite, worker), testutil.Spec("an3", "2", "v2 call:search", "search"), factoryInput("second"))
	assert.Equal(t, "v2 call:search | searched", out.Text())
	assert.Zero(t, store.Loads(agents.AgentRef{Name: "an3", Version: "1"}))
}

// A handoff loads its target in an activity when the model hands off, so a cycle is fine.
func TestFactoryWorkflow_LoadsHandoffTargetsWhenUsed(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	store := testutil.NewSpecStore()
	billing := testutil.Spec("billing", "9", "billing@9 call:invoice", "invoice")
	billing.Handoffs = []agents.HandoffSpec{{Name: "an3", Version: "4", Description: "back"}}
	store.Put(billing)
	root := testutil.Spec("an3", "4", "root handoff:billing")
	root.Handoffs = []agents.HandoffSpec{{Name: "billing", Version: "9", Description: "bills"}}
	store.Put(root)

	worker := newFactoryWorker(t, store.Factory(newHistory(), testutil.NewTextTool("invoice", "invoice-77")))
	out := runSpec(t, newFactoryEnv(&suite, worker), root, factoryInput("bill me"))
	assert.True(t, strings.HasPrefix(out.Text(), "billing@9 call:invoice"), "the specialist answers")
	assert.True(t, strings.HasSuffix(out.Text(), "| invoice-77"), "its tool runs through the shared tool activity")
	assert.Equal(t, 1, store.Loads(billing.Ref()))
	assert.Zero(t, store.Loads(root.Ref()), "the root came with the run")
}

// Specs the worker cannot serve fail the run instead of retrying.
func TestFactoryWorkflow_FailsFastForSpecsItCannotRun(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	worker := newFactoryWorker(t, testutil.NewSpecStore().Factory(newHistory()))

	orphan := testutil.Spec("an3", "1", "handoff:ghost")
	orphan.Handoffs = []agents.HandoffSpec{{Name: "ghost", Version: "1"}}
	assert.ErrorContains(t, runSpecError(t, newFactoryEnv(&suite, worker), orphan), "agent not found")

	assert.ErrorContains(t, runSpecError(t, newFactoryEnv(&suite, worker), testutil.Spec("an3", "1", "x", "deleted_tool")), "unknown tool")

	unknownModel := testutil.Spec("an3", "1", "x")
	unknownModel.Model = "Nobody/none"
	assert.ErrorContains(t, runSpecError(t, newFactoryEnv(&suite, worker), unknownModel), "unknown model")
}

// headerWriter collects what a propagator writes, to hand the test environment as a header.
type headerWriter map[string]*commonpb.Payload

func (h headerWriter) Set(key string, value *commonpb.Payload) { h[key] = value }

// Values a ConfigStore reads with gateway.GetContext must reach every activity.
func TestFactoryWorkflow_CarriesGatewayContextToActivities(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	propagator := temporal_runtime.NewProviderConfigKeyPropagator()
	suite.SetContextPropagators([]workflow.ContextPropagator{propagator})
	worker := newFactoryWorker(t, testutil.NewSpecStore().Factory(newHistory(), testutil.NewGatewayContextTool("whoami", "amg_id")))
	env := newFactoryEnv(&suite, worker)

	header := headerWriter{}
	callerCtx := gateway.AddContext(context.Background(), map[string]string{"amg_id": "amg-42"})
	require.NoError(t, propagator.Inject(callerCtx, header))
	env.SetHeader(&commonpb.Header{Fields: header})

	out := runSpec(t, env, testutil.Spec("an3", "1", "call:whoami", "whoami"), factoryInput("who"))
	assert.Equal(t, "call:whoami | amg_id=amg-42", out.Text())
}

type headerReader map[string]*commonpb.Payload

func (h headerReader) Get(key string) (*commonpb.Payload, bool) { p, ok := h[key]; return p, ok }

func (h headerReader) ForEachKey(fn func(string, *commonpb.Payload) error) error {
	for k, v := range h {
		if err := fn(k, v); err != nil {
			return err
		}
	}
	return nil
}

func TestPropagator_RoundTripsKeyAndGatewayContext(t *testing.T) {
	propagator := temporal_runtime.NewProviderConfigKeyPropagator()
	ctx := gateway.WithProviderConfigKey(context.Background(), "vk-1")
	ctx = gateway.AddContext(ctx, map[string]string{"amg_id": "amg-42", "tenant": "t"})

	header := headerWriter{}
	require.NoError(t, propagator.Inject(ctx, header))
	got, err := propagator.Extract(context.Background(), headerReader(header))
	require.NoError(t, err)
	assert.Equal(t, "vk-1", gateway.ProviderConfigKeyFromContext(got))
	assert.Equal(t, map[string]string{"amg_id": "amg-42", "tenant": "t"}, gateway.GetContext(got))

	empty := headerWriter{}
	require.NoError(t, propagator.Inject(context.Background(), empty))
	assert.Empty(t, empty, "nothing is written when there is nothing to carry")
}

// A factory agent's background task waits in the shared background workflow,
// and the run its result wakes starts from the spec the task began under.
func TestFactoryBackgroundTask_WakesTheAgentWithItsSpec(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	worker := newFactoryWorker(t, testutil.NewSpecStore().Factory(newHistory(), testutil.NewBackgroundTool("index", "task-1", "started", "indexed 9 docs")))
	spec := testutil.Spec("indexer", "2", "call:index", "index")

	// The run starts the task's workflow with its spec.
	env := newFactoryEnv(&suite, worker, temporal_runtime.FactoryBackgroundTaskWorkflowName)
	var started *temporal_runtime.BackgroundTaskInput
	env.RegisterWorkflowWithOptions(func(_ workflow.Context, in *temporal_runtime.BackgroundTaskInput) error {
		started = in
		return nil
	}, workflow.RegisterOptions{Name: temporal_runtime.FactoryBackgroundTaskWorkflowName})
	in := factoryInput("index")
	in.StreamID = "thread-stream"
	runSpec(t, env, spec, in)
	require.NotNil(t, started)
	assert.Equal(t, &spec, started.Spec)

	// The task's workflow awaits the real tool, then wakes the agent from that spec.
	env = newFactoryEnv(&suite, worker, temporal_runtime.FactoryAgentWorkflowName)
	var wokeSpec *agents.AgentSpec
	var wokeWith *agents.AgentInput
	env.RegisterWorkflowWithOptions(func(_ workflow.Context, spec *agents.AgentSpec, in *agents.AgentInput) (*agents.AgentOutput, error) {
		wokeSpec, wokeWith = spec, in
		return &agents.AgentOutput{}, nil
	}, workflow.RegisterOptions{Name: temporal_runtime.FactoryAgentWorkflowName})
	env.ExecuteWorkflow(temporal_runtime.FactoryBackgroundTaskWorkflowName, started)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, &spec, wokeSpec)
	require.NotNil(t, wokeWith)
	data, err := json.Marshal(wokeWith.Message)
	require.NoError(t, err)
	assert.Contains(t, string(data), "indexed 9 docs")
}

type countingSummarizer struct {
	history.HistorySummarizer
	decisions atomic.Int32
}

func (s *countingSummarizer) ShouldSummarize(ctx context.Context, ids map[string]string, msgs []messages.Message, tokens int) (bool, error) {
	s.decisions.Add(1)
	return s.HistorySummarizer.ShouldSummarize(ctx, ids, msgs, tokens)
}

type countingFilter struct{ calls atomic.Int32 }

func (f *countingFilter) Filter(_ context.Context, msgs []messages.Message, _ string) []messages.Message {
	f.calls.Add(1)
	return msgs
}

// The worker's history settings apply to every factory agent.
func TestFactoryWorkflow_UsesTheWorkersSummarizerAndFilter(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	summarizer := &countingSummarizer{HistorySummarizer: summariser.NewSlidingWindowHistorySummarizer(&summariser.SlidingWindowHistorySummarizerOptions{KeepCount: 2})}
	filter := &countingFilter{}
	h := history.NewConversationManager(history.NewInMemoryConversationPersistence(), history.WithSummarizer(summarizer), history.WithMessageFilter(filter))
	worker := newFactoryWorker(t, testutil.NewSpecStore().Factory(h, testutil.NewTextTool("lookup", "found")))

	out := runSpec(t, newFactoryEnv(&suite, worker), testutil.Spec("an3", "1", "call:lookup", "lookup"), factoryInput("summarize"))
	assert.Equal(t, "call:lookup | found", out.Text())
	assert.NotZero(t, summarizer.decisions.Load())
	assert.NotZero(t, filter.calls.Load())
}
