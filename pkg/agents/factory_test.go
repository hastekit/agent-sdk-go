package agents_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/internal/testutil"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func optionsFor(name, prompt string, handoffs ...*agents.Handoff) func() *agents.AgentOptions {
	return func() *agents.AgentOptions {
		return &agents.AgentOptions{
			Name:        name,
			LLM:         testutil.EchoProvider{},
			Instruction: prompts.New(prompt),
			History:     history.NewConversationManager(history.NewInMemoryConversationPersistence()),
			Handoffs:    handoffs,
		}
	}
}

func TestCachedAgentFactory_BuildsEachRefOnceAndEvictsTheOldest(t *testing.T) {
	store := testutil.NewMapFactory()
	a, b, c := agents.AgentRef{Name: "a", Version: "1"}, agents.AgentRef{Name: "b", Version: "1"}, agents.AgentRef{Name: "c", Version: "1"}
	for _, ref := range []agents.AgentRef{a, b, c} {
		store.Put(ref, optionsFor(ref.Name, ref.Name))
	}
	cache := agents.NewCachedAgentFactory(store, 2)

	for _, ref := range []agents.AgentRef{a, b, a, c, a} {
		_, err := cache.Build(t.Context(), ref)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, store.Builds(a), "a stayed recent, so it was never rebuilt")
	assert.Equal(t, 1, store.Builds(c))

	_, err := cache.Build(t.Context(), b)
	require.NoError(t, err)
	assert.Equal(t, 2, store.Builds(b), "b was the least recent when c arrived, so it was evicted")
	assert.Same(t, cache, agents.NewCachedAgentFactory(cache, 10), "wrapping a cache again reuses it")
}

func TestCachedAgentFactory_ConcurrentBuildsOfOneRefShareOneBuild(t *testing.T) {
	var mu sync.Mutex
	builds := 0
	cache := agents.NewCachedAgentFactory(agents.AgentFactoryFunc(func(context.Context, agents.AgentRef) (*agents.AgentOptions, error) {
		mu.Lock()
		builds++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return optionsFor("a", "a")(), nil
	}), 0)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.Build(context.Background(), agents.AgentRef{Name: "a", Version: "1"})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, builds)
}

func TestNewFactoryAgent_WiresAHandoffCycleByRef(t *testing.T) {
	store := testutil.NewMapFactory()
	root := agents.AgentRef{Name: "root", Version: "1"}
	specialist := agents.AgentRef{Name: "specialist", Version: "7"}
	store.Put(root, optionsFor("root", "root", &agents.Handoff{Name: "specialist", Description: "does the work", Version: "7"}))
	store.Put(specialist, optionsFor("specialist", "specialist", &agents.Handoff{Name: "root", Description: "hands back", Version: "1"}))

	agent, err := agents.NewFactoryAgent(t.Context(), store, root, nil)
	require.NoError(t, err)

	assert.True(t, agent.FactoryBuilt())
	assert.Equal(t, root, agent.Ref())
	assert.NotEmpty(t, agent.PrepareHandoffTools(t.Context()))
	assert.Equal(t, 1, store.Builds(root), "the cycle back to root reuses the agent already built")
	assert.Equal(t, 1, store.Builds(specialist))
}

func TestNewFactoryAgent_LeavesTheStoredOptionsAlone(t *testing.T) {
	stored := &agents.AgentOptions{Name: "a", LLM: testutil.EchoProvider{}, Output: map[string]any{"type": "object"}}
	factory := agents.AgentFactoryFunc(func(context.Context, agents.AgentRef) (*agents.AgentOptions, error) { return stored, nil })

	for range 2 {
		_, err := agents.NewFactoryAgent(t.Context(), factory, agents.AgentRef{Name: "a", Version: "1"}, func(o *agents.AgentOptions) { o.MaxLoops = new(int) })
		require.NoError(t, err)
	}
	assert.Nil(t, stored.Parameters.Text, "NewAgent writes the output format to its own copy")
	assert.Nil(t, stored.History)
	assert.Nil(t, stored.MaxLoops, "configure applies to the copy")
	assert.Empty(t, stored.Version)
}

func TestNewFactoryAgent_RejectsAFactoryThatBuildsAnotherAgent(t *testing.T) {
	factory := agents.AgentFactoryFunc(func(context.Context, agents.AgentRef) (*agents.AgentOptions, error) {
		return optionsFor("other", "x")(), nil
	})
	_, err := agents.NewFactoryAgent(t.Context(), factory, agents.AgentRef{Name: "a"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"other"`)
}

func TestDescribeAgentGraph_KeepsTheShapeAndDropsMissingTargets(t *testing.T) {
	store := testutil.NewMapFactory()
	root := agents.AgentRef{Name: "root", Version: "3"}
	loops := 4
	store.Put(root, func() *agents.AgentOptions {
		o := optionsFor("root", "root",
			&agents.Handoff{Name: "specialist", Version: "1", Description: "exists"},
			&agents.Handoff{Name: "ghost", Version: "1", Description: "was deleted"},
		)()
		o.MaxLoops = &loops
		o.StickyHandoff = true
		o.Parameters = responses.Parameters{Temperature: new(float64)}
		o.Tools = []agents.Tool{testutil.NewTextTool("lookup", "x"), testutil.NewBackgroundTool("index", "task-1", "started", "done")}
		o.History.MessageAttribution = true
		return o
	})
	store.Put(agents.AgentRef{Name: "specialist", Version: "1"}, optionsFor("specialist", "specialist"))

	specs, err := agents.DescribeAgentGraph(t.Context(), store, root)
	require.NoError(t, err)
	require.Len(t, specs, 2)

	spec := specs[0]
	assert.Equal(t, root, spec.Ref)
	assert.Equal(t, []agents.HandoffSpec{{Ref: agents.AgentRef{Name: "specialist", Version: "1"}, Description: "exists"}}, spec.Handoffs)
	require.Len(t, spec.Tools, 2)
	assert.Equal(t, "lookup", spec.Tools[0].Key())
	assert.False(t, spec.Tools[0].Background)
	assert.True(t, spec.Tools[1].Background, "the loop has to know a tool may answer with a task id")
	assert.True(t, spec.HasInstruction)
	assert.True(t, spec.MessageAttribution)
	assert.True(t, spec.SteeringNotices)
	assert.Equal(t, &loops, spec.MaxLoops)

	// The spec is journaled, so it must survive a JSON round trip unchanged.
	data, err := json.Marshal(specs)
	require.NoError(t, err)
	var decoded []agents.AgentSpec
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, specs, decoded)
}

func TestDescribeAgentGraph_FailsForAnUnknownRoot(t *testing.T) {
	_, err := agents.DescribeAgentGraph(t.Context(), testutil.NewMapFactory(), agents.AgentRef{Name: "nobody"})
	require.ErrorIs(t, err, agents.ErrAgentNotFound)
}
