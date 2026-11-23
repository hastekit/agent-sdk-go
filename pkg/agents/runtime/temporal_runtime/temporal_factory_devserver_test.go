package temporal_runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentfactory"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const taskQueue = "AgentWorkflowTaskQueue"

// startFactoryWorker runs one worker process: the factory workflow and its activities over factory.
func startFactoryWorker(t *testing.T, c client.Client, factory *agents.AgentFactory, broker agents.StreamBroker) worker.Worker {
	t.Helper()
	w := worker.New(c, taskQueue, worker.Options{WorkerStopTimeout: 100 * time.Millisecond})
	fw, err := temporal_runtime.NewFactoryAgentWorkflow(factory, broker)
	require.NoError(t, err)
	for name, fn := range fw.GetActivities() {
		w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	for name, fn := range fw.GetWorkflows() {
		w.RegisterWorkflowWithOptions(fn, workflow.RegisterOptions{Name: name})
	}
	require.NoError(t, w.Start())
	return w
}

func buildRef(t *testing.T, factory *agents.AgentFactory, rt agents.Runtime, broker agents.StreamBroker, ref agents.AgentRef) *agents.Agent {
	t.Helper()
	agent, err := agentfactory.Build(t.Context(), factory, ref, func(o *agents.AgentOptions) {
		o.Runtime = rt
		o.StreamBroker = broker
	})
	require.NoError(t, err)
	return agent
}

func runRef(t *testing.T, factory *agents.AgentFactory, rt agents.Runtime, broker agents.StreamBroker, ref agents.AgentRef, text string) *agents.AgentOutput {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	out, err := buildRef(t, factory, rt, broker, ref).Run(ctx, factoryInput(text))
	require.NoError(t, err)
	return out
}

// Against a real server: specs stored after the worker starts run without a restart,
// and an in-flight run survives a worker restart and a replay on the spec it started with.
func TestFactoryWorker_RestartsAndReplaysOnTheSpecItStartedWith(t *testing.T) {
	hostPort := testutil.StartTemporalDevServer(t)
	c, err := client.Dial(client.Options{HostPort: hostPort})
	require.NoError(t, err)
	defer c.Close()

	broker := streambroker.NewMemoryStreamBroker()
	rt := temporal_runtime.NewTemporalRuntime(c, broker)
	conversations := history.NewConversationManager(history.NewInMemoryConversationPersistence())

	// The worker starts before any spec exists.
	started := make(chan struct{})
	var once sync.Once
	blockingWait := testutil.NewFuncTool("wait", func(ctx context.Context, _ *agents.ToolCall) (string, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return "", ctx.Err()
	})
	store := testutil.NewSpecStore()
	first := store.Factory(conversations, testutil.NewTextTool("lookup", "found"), blockingWait)
	w1 := startFactoryWorker(t, c, first, broker)

	store.Put(testutil.Spec("an3", "1", "v1 call:lookup", "lookup"))
	assert.Equal(t, "v1 call:lookup | found", runRef(t, first, rt, broker, agents.AgentRef{Name: "an3"}, "a").Text())
	store.Put(testutil.Spec("an3", "2", "v2 answers"))
	assert.Equal(t, "v2 answers", runRef(t, first, rt, broker, agents.AgentRef{Name: "an3"}, "b").Text(), "the next run picks up the new version")

	// A run of slow@1 blocks in its tool on the first worker.
	store.Put(testutil.Spec("slow", "1", "slow@1 call:wait", "wait"))
	agent := buildRef(t, first, rt, broker, agents.AgentRef{Name: "slow"})
	in := factoryInput("c")
	in.StreamID = "slow-run"
	done := make(chan *agents.AgentOutput, 1)
	go func() {
		out, err := agent.Run(context.Background(), in)
		assert.NoError(t, err)
		done <- out
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the tool never started")
	}

	// Restart: a new process whose store has moved on to slow@2, and whose wait tool answers.
	w1.Stop()
	store.Put(testutil.Spec("slow", "2", "slow@2 answers"))
	loadsBefore := store.Loads(agents.AgentRef{Name: "slow"})
	second := store.Factory(conversations, testutil.NewTextTool("lookup", "found"), testutil.NewTextTool("wait", "waited"))
	w2 := startFactoryWorker(t, c, second, broker)
	defer w2.Stop()

	select {
	case out := <-done:
		assert.Equal(t, "slow@1 call:wait | waited", out.Text(), "the run finishes on the spec it started with")
	case <-time.After(90 * time.Second):
		t.Fatal("the run did not finish after the restart")
	}
	assert.Equal(t, loadsBefore, store.Loads(agents.AgentRef{Name: "slow"}), "the restarted worker never asked the store")

	// The spec is the recorded input, so replay needs nothing from the store, which has moved on.
	iter := c.GetWorkflowHistory(t.Context(), in.StreamID, "", false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	recorded := &historypb.History{}
	for iter.HasNext() {
		event, err := iter.Next()
		require.NoError(t, err)
		recorded.Events = append(recorded.Events, event)
	}
	require.NotEmpty(t, recorded.Events)
	input := recorded.Events[0].GetWorkflowExecutionStartedEventAttributes().GetInput().GetPayloads()
	require.NotEmpty(t, input)
	assert.Contains(t, string(input[0].GetData()), "slow@1 call:wait", "the run's first input is the spec itself")

	replayer := worker.NewWorkflowReplayer()
	fw, err := temporal_runtime.NewFactoryAgentWorkflow(second, broker)
	require.NoError(t, err)
	for name, fn := range fw.GetWorkflows() {
		replayer.RegisterWorkflowWithOptions(fn, workflow.RegisterOptions{Name: name})
	}
	require.NoError(t, replayer.ReplayWorkflowHistory(nil, recorded))
	assert.Equal(t, loadsBefore, store.Loads(agents.AgentRef{Name: "slow"}), "replay never consults the store")
}
