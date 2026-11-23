package temporal_runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history/summariser"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
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
func startFactoryWorker(t *testing.T, c client.Client, factory agents.AgentFactory, broker agents.StreamBroker) worker.Worker {
	t.Helper()
	w := worker.New(c, taskQueue, worker.Options{WorkerStopTimeout: 100 * time.Millisecond})
	fw := temporal_runtime.NewFactoryAgentWorkflow(factory, broker)
	for name, fn := range fw.GetActivities() {
		w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	for name, fn := range fw.GetWorkflows() {
		w.RegisterWorkflowWithOptions(fn, workflow.RegisterOptions{Name: name})
	}
	require.NoError(t, w.Start())
	return w
}

func runRef(t *testing.T, factory agents.AgentFactory, rt agents.Runtime, broker agents.StreamBroker, ref agents.AgentRef, text string) *agents.AgentOutput {
	t.Helper()
	agent, err := agents.NewFactoryAgent(t.Context(), factory, ref, func(o *agents.AgentOptions) {
		o.Runtime = rt
		o.StreamBroker = broker
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	out, err := agent.Run(ctx, factoryInput(text))
	require.NoError(t, err)
	return out
}

// Against a real server: agents added and versioned after the worker starts run
// without a restart, and an in-flight run survives a worker restart on its own version.
func TestFactoryWorker_RestartsAndReplaysOnThePinnedVersion(t *testing.T) {
	hostPort := testutil.StartTemporalDevServer(t)
	c, err := client.Dial(client.Options{HostPort: hostPort})
	require.NoError(t, err)
	defer c.Close()

	broker := streambroker.NewMemoryStreamBroker()
	rt := temporal_runtime.NewTemporalRuntime(c, broker)
	persistence := history.NewInMemoryConversationPersistence()
	config := func(name, prompt string, tools ...agents.Tool) func() *agents.AgentOptions {
		return func() *agents.AgentOptions {
			return &agents.AgentOptions{Name: name, LLM: testutil.EchoProvider{}, Instruction: prompts.New(prompt),
				History: history.NewConversationManager(persistence), Tools: tools}
		}
	}

	// The worker starts before any agent exists.
	first := testutil.NewMapFactory()
	w1 := startFactoryWorker(t, c, first, broker)

	v1 := agents.AgentRef{Name: "an3", Version: "1"}
	first.Put(v1, config("an3", "v1 call:lookup", testutil.NewTextTool("lookup", "found-v1")))
	assert.Equal(t, "v1 call:lookup | found-v1", runRef(t, first, rt, broker, v1, "a").Text())

	v2 := agents.AgentRef{Name: "an3", Version: "2"}
	first.Put(v2, config("an3", "v2 call:search", testutil.NewTextTool("search", "found-v2")))
	assert.Equal(t, "v2 call:search | found-v2", runRef(t, first, rt, broker, v2, "b").Text())

	// A run of slow@1 blocks in its tool on the first worker.
	started := make(chan struct{})
	var once sync.Once
	slow := agents.AgentRef{Name: "slow", Version: "1"}
	first.Put(slow, config("slow", "slow@1 call:wait", testutil.NewFuncTool("wait", func(ctx context.Context, _ *agents.ToolCall) (string, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return "", ctx.Err()
	})))
	agent, err := agents.NewFactoryAgent(t.Context(), first, slow, func(o *agents.AgentOptions) { o.Runtime = rt; o.StreamBroker = broker })
	require.NoError(t, err)
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

	// Restart: a new process whose store has moved on to slow@2 but still holds slow@1.
	w1.Stop()
	second := testutil.NewMapFactory()
	second.Put(slow, config("slow", "slow@1 call:wait", testutil.NewTextTool("wait", "waited")))
	second.Put(agents.AgentRef{Name: "slow", Version: "2"}, config("slow", "slow@2 call:other", testutil.NewTextTool("other", "nope")))
	w2 := startFactoryWorker(t, c, second, broker)
	defer w2.Stop()

	select {
	case out := <-done:
		assert.Equal(t, "slow@1 call:wait | waited", out.Text(), "the run finishes on the version it started with")
	case <-time.After(90 * time.Second):
		t.Fatal("the run did not finish after the restart")
	}
	assert.Zero(t, second.Builds(agents.AgentRef{Name: "slow", Version: "2"}), "nothing asked for the new version")

	// Replay the finished history on a worker whose stored slow@1 has since changed shape.
	// Replay must not depend on the factory: the spec comes from the journaled DescribeAgent.
	changed := testutil.NewMapFactory()
	changed.Put(slow, func() *agents.AgentOptions {
		o := config("slow", "slow@1 rewritten", testutil.NewTextTool("extra", "x"))()
		o.History = history.NewConversationManager(persistence,
			history.WithSummarizer(summariser.NewSlidingWindowHistorySummarizer(&summariser.SlidingWindowHistorySummarizerOptions{KeepCount: 1})),
			history.WithMessageFilter(keepAll{}))
		return o
	})
	replayer := worker.NewWorkflowReplayer()
	for name, fn := range temporal_runtime.NewFactoryAgentWorkflow(changed, broker).GetWorkflows() {
		replayer.RegisterWorkflowWithOptions(fn, workflow.RegisterOptions{Name: name})
	}
	iter := c.GetWorkflowHistory(t.Context(), in.StreamID, "", false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	recorded := &historypb.History{}
	describes := 0
	for iter.HasNext() {
		event, err := iter.Next()
		require.NoError(t, err)
		if attrs := event.GetActivityTaskScheduledEventAttributes(); attrs != nil && attrs.GetActivityType().GetName() == "hastekit_Factory_DescribeAgentActivity" {
			describes++
		}
		recorded.Events = append(recorded.Events, event)
	}
	require.NotEmpty(t, recorded.Events)
	assert.Equal(t, 1, describes, "the agent is described once, when the run starts")
	require.NoError(t, replayer.ReplayWorkflowHistory(nil, recorded))
	assert.Zero(t, changed.Builds(slow), "replay never consults the factory")
}

type keepAll struct{}

func (keepAll) Filter(_ context.Context, msgs []messages.Message, _ string) []messages.Message {
	return msgs
}
