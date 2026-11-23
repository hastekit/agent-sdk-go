package temporal_runtime_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history/summariser"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
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

var sharedPersistence = history.NewInMemoryConversationPersistence()

func factoryConfig(name, prompt string, tools []agents.Tool, handoffs ...*agents.Handoff) func() *agents.AgentOptions {
	return func() *agents.AgentOptions {
		return &agents.AgentOptions{
			Name:        name,
			LLM:         testutil.EchoProvider{},
			Instruction: prompts.New(prompt),
			History:     history.NewConversationManager(sharedPersistence),
			Tools:       tools,
			Handoffs:    handoffs,
		}
	}
}

func factoryInput(text string) *agents.AgentInput {
	return &agents.AgentInput{Namespace: "ns", ThreadID: "thread-" + text, Message: messages.New("user", []responses.InputMessageUnion{{
		OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)}},
	}})}
}

// newFactoryEnv registers one factory worker's workflows and activities, as Serve does.
func newFactoryEnv(suite *testsuite.WorkflowTestSuite, worker *temporal_runtime.FactoryAgentWorkflow) *testsuite.TestWorkflowEnvironment {
	env := suite.NewTestWorkflowEnvironment()
	for name, fn := range worker.GetActivities() {
		env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	for name, fn := range worker.GetWorkflows() {
		env.RegisterWorkflowWithOptions(fn, workflow.RegisterOptions{Name: name})
	}
	return env
}

func runFactory(t *testing.T, env *testsuite.TestWorkflowEnvironment, ref agents.AgentRef, in *agents.AgentInput) *agents.AgentOutput {
	t.Helper()
	env.ExecuteWorkflow(temporal_runtime.FactoryAgentWorkflowName, ref, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out agents.AgentOutput
	require.NoError(t, env.GetWorkflowResult(&out))
	return &out
}

// One worker, registered before any agent exists: an agent added later runs, a new
// version applies from the next run, and an old ref still builds its own version.
func TestFactoryWorkflow_ServesAgentsAddedAfterTheWorkerStarted(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	store := testutil.NewMapFactory()
	worker := temporal_runtime.NewFactoryAgentWorkflow(store, streambroker.NewMemoryStreamBroker())

	v1 := agents.AgentRef{Name: "an3", Version: "1"}
	store.Put(v1, factoryConfig("an3", "v1 call:lookup", []agents.Tool{testutil.NewTextTool("lookup", "found-v1")}))
	out := runFactory(t, newFactoryEnv(&suite, worker), v1, factoryInput("first"))
	assert.Equal(t, "v1 call:lookup | found-v1", out.Text())

	v2 := agents.AgentRef{Name: "an3", Version: "2"}
	store.Put(v2, factoryConfig("an3", "v2 call:search", []agents.Tool{testutil.NewTextTool("search", "found-v2")}))
	out = runFactory(t, newFactoryEnv(&suite, worker), v2, factoryInput("second"))
	assert.Equal(t, "v2 call:search | found-v2", out.Text())

	out = runFactory(t, newFactoryEnv(&suite, worker), v1, factoryInput("third"))
	assert.Equal(t, "v1 call:lookup | found-v1", out.Text(), "a ref keeps building its own version")
}

func TestFactoryWorkflow_HandsOffBetweenFactoryAgents(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	store := testutil.NewMapFactory()
	root := agents.AgentRef{Name: "an3", Version: "4"}
	store.Put(root, factoryConfig("an3", "root handoff:billing", nil, &agents.Handoff{Name: "billing", Description: "bills", Version: "9"}))
	// The specialist can hand back, so the graph is a cycle.
	store.Put(agents.AgentRef{Name: "billing", Version: "9"}, factoryConfig("billing", "billing@9 call:invoice",
		[]agents.Tool{testutil.NewTextTool("invoice", "invoice-77")}, &agents.Handoff{Name: "an3", Description: "back", Version: "4"}))

	worker := temporal_runtime.NewFactoryAgentWorkflow(store, streambroker.NewMemoryStreamBroker())
	out := runFactory(t, newFactoryEnv(&suite, worker), root, factoryInput("bill me"))
	assert.Equal(t, "billing@9 call:invoice | invoice-77", out.Text(), "the specialist's own tool runs through the shared tool activity")
}

func TestFactoryWorkflow_FailsFastForAnUnknownRef(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	worker := temporal_runtime.NewFactoryAgentWorkflow(testutil.NewMapFactory(), streambroker.NewMemoryStreamBroker())
	env := newFactoryEnv(&suite, worker)
	env.ExecuteWorkflow(temporal_runtime.FactoryAgentWorkflowName, agents.AgentRef{Name: "ghost", Version: "1"}, factoryInput("x"))
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	assert.Contains(t, env.GetWorkflowError().Error(), "agent not found")
}

// headerWriter collects what a propagator writes, to hand the test environment as a header.
type headerWriter map[string]*commonpb.Payload

func (h headerWriter) Set(key string, value *commonpb.Payload) { h[key] = value }

// Values a ConfigStore reads with gateway.GetContext must reach every activity.
func TestFactoryWorkflow_CarriesGatewayContextToActivities(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	propagator := temporal_runtime.NewProviderConfigKeyPropagator()
	suite.SetContextPropagators([]workflow.ContextPropagator{propagator})

	store := testutil.NewMapFactory()
	ref := agents.AgentRef{Name: "an3", Version: "1"}
	store.Put(ref, factoryConfig("an3", "call:whoami", []agents.Tool{testutil.NewGatewayContextTool("whoami", "amg_id")}))
	env := newFactoryEnv(&suite, temporal_runtime.NewFactoryAgentWorkflow(store, streambroker.NewMemoryStreamBroker()))

	header := headerWriter{}
	callerCtx := gateway.AddContext(context.Background(), map[string]string{"amg_id": "amg-42"})
	require.NoError(t, propagator.Inject(callerCtx, header))
	env.SetHeader(&commonpb.Header{Fields: header})

	out := runFactory(t, env, ref, factoryInput("who"))
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

// The task's wait runs on the tool agent's ref; a woken run enters at the owner's pinned ref.
func TestFactoryBackgroundWorkflow_WaitsOnTheToolAgentAndWakesTheOwner(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	store := testutil.NewMapFactory()
	owner := agents.AgentRef{Name: "an3", Version: "1"}
	indexer := agents.AgentRef{Name: "indexer", Version: "2"}
	store.Put(owner, factoryConfig("an3", "owner", nil))
	store.Put(indexer, factoryConfig("indexer", "indexer", []agents.Tool{testutil.NewBackgroundTool("index", "task-1", "started", "indexed 9 docs")}))
	worker := temporal_runtime.NewFactoryAgentWorkflow(store, streambroker.NewMemoryStreamBroker())

	env := suite.NewTestWorkflowEnvironment()
	for name, fn := range worker.GetActivities() {
		env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	env.RegisterWorkflowWithOptions(worker.ExecuteBackgroundTask, workflow.RegisterOptions{Name: temporal_runtime.FactoryBackgroundTaskWorkflowName})
	var ranRef agents.AgentRef
	var ranWith *agents.AgentInput
	env.RegisterWorkflowWithOptions(func(_ workflow.Context, ref agents.AgentRef, in *agents.AgentInput) (*agents.AgentOutput, error) {
		ranRef, ranWith = ref, in
		return &agents.AgentOutput{}, nil
	}, workflow.RegisterOptions{Name: temporal_runtime.FactoryAgentWorkflowName})

	env.ExecuteWorkflow(temporal_runtime.FactoryBackgroundTaskWorkflowName, &temporal_runtime.FactoryBackgroundTaskInput{
		Owner: owner, ToolAgent: indexer, ToolKey: "index",
		Ref: agents.BackgroundTaskRef{TaskID: "task-1", CallID: "call_index", ToolName: "index", AgentName: "an3",
			Namespace: "ns", ThreadID: "thread-1", TaskStreamID: "task-stream", ThreadStreamID: "thread-stream"},
	})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, owner, ranRef, "the run the result starts is pinned to the owner's version")
	require.NotNil(t, ranWith)
	assert.Equal(t, "thread-stream", ranWith.StreamID)
	data, err := json.Marshal(ranWith.Message)
	require.NoError(t, err)
	assert.Contains(t, string(data), "indexed 9 docs", "the tool agent's real tool was awaited")
	assert.NotZero(t, store.Builds(indexer))
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

// The summarizer and filter live on the worker; the decision comes back through a local activity.
func TestFactoryWorkflow_RunsTheWorkersSummarizerAndFilter(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	summarizer := &countingSummarizer{HistorySummarizer: summariser.NewSlidingWindowHistorySummarizer(&summariser.SlidingWindowHistorySummarizerOptions{KeepCount: 2})}
	filter := &countingFilter{}
	store := testutil.NewMapFactory()
	ref := agents.AgentRef{Name: "an3", Version: "1"}
	store.Put(ref, func() *agents.AgentOptions {
		o := factoryConfig("an3", "call:lookup", []agents.Tool{testutil.NewTextTool("lookup", "found")})()
		o.History = history.NewConversationManager(sharedPersistence, history.WithSummarizer(summarizer), history.WithMessageFilter(filter))
		return o
	})
	out := runFactory(t, newFactoryEnv(&suite, temporal_runtime.NewFactoryAgentWorkflow(store, streambroker.NewMemoryStreamBroker())), ref, factoryInput("summarize"))
	assert.Equal(t, "call:lookup | found", out.Text())
	assert.NotZero(t, summarizer.decisions.Load())
	assert.NotZero(t, filter.calls.Load())
}
