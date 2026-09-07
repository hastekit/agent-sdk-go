package temporal_runtime

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The workflow rebuilds the agent on the far side of the boundary, and what it
// does not copy simply stops applying — silently, since nothing fails. Sticky
// routing stopped happening that way: the option was added to AgentOptions and
// the rebuild was never told.
//
// The rebuild now starts from the agent's own options and replaces only what
// has to run as a workflow step, so a new option carries by default. This
// pins the two that were lost.
func TestProxyAgent_CarriesTheAgentsOwnOptions(t *testing.T) {
	configs := map[string]*agents.AgentOptions{
		"Specialist": {
			Name:    "Specialist",
			History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
		},
	}
	options := &agents.AgentOptions{
		Name:          "Root",
		History:       history.NewConversationManager(history.NewInMemoryConversationPersistence()),
		StickyHandoff: true,
		SingleTurn:    true,
		Handoffs: []*agents.Handoff{
			agents.NewHandoff("Specialist", "does the specialist things", nil),
		},
	}
	configs["Root"] = options

	// No workflow context needed: the proxies only store it, and nothing here
	// runs a step.
	proxy := NewTemporalAgent(configs, options, streambroker.NewMemoryStreamBroker()).
		newTemporalProxyAgent(nil)

	require.NotNil(t, proxy)
	assert.True(t, proxy.StickyHandoff(), "a turn must still resume in the specialist it ended in")
	assert.True(t, proxy.SingleTurn())
	assert.Equal(t, "Root", proxy.Name)
}

// An edge added after construction has to reach the workflow rebuild.
//
// AddHandoffs appended to the agent alone, and the rebuild reads the options
// the runtime registered — so a handoff wired up this way worked in process
// and was simply absent from every durable run.
func TestProxyAgent_CarriesHandoffsAddedAfterConstruction(t *testing.T) {
	specialist := &agents.AgentOptions{
		Name:    "Specialist",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}
	root := &agents.AgentOptions{
		Name:    "Root",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}
	configs := map[string]*agents.AgentOptions{"Root": root, "Specialist": specialist}

	// The runtime registers the options; the caller then wires the edge up on
	// the agent, which is the pair that used to disagree.
	rootAgent := agents.NewAgent(root)
	rootAgent.AddHandoffs(agents.NewHandoff("Specialist", "does the specialist things", agents.NewAgent(specialist)))

	proxy := NewTemporalAgent(configs, root, streambroker.NewMemoryStreamBroker()).
		newTemporalProxyAgent(nil)

	require.NotNil(t, proxy)
	assert.NotEmpty(t, proxy.PrepareHandoffTools(context.Background()),
		"the workflow rebuild must offer transfer_to_agent, or the edge does not exist durably")
}

// A target the runtime never heard of is dropped rather than crashing the
// workflow, which Temporal would otherwise retry forever.
func TestProxyAgent_SkipsAnUnregisteredHandoffTarget(t *testing.T) {
	root := &agents.AgentOptions{
		Name:    "Root",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
		Handoffs: []*agents.Handoff{
			agents.NewHandoff("Nowhere", "never registered", nil),
		},
	}

	assert.NotPanics(t, func() {
		proxy := NewTemporalAgent(map[string]*agents.AgentOptions{"Root": root}, root,
			streambroker.NewMemoryStreamBroker()).newTemporalProxyAgent(nil)
		require.NotNil(t, proxy)
	})
}

// Handoffs are a graph, not a tree. A specialist that can hand back to the
// agent that called it is the ordinary shape, and rebuilding each target in
// turn walked that cycle until the stack ran out.
func TestProxyAgent_HandlesAHandoffCycle(t *testing.T) {
	root := &agents.AgentOptions{
		Name:    "Root",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}
	specialist := &agents.AgentOptions{
		Name:    "Specialist",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}
	configs := map[string]*agents.AgentOptions{"Root": root, "Specialist": specialist}

	// Root hands off to the specialist, and the specialist hands back.
	root.Handoffs = []*agents.Handoff{agents.NewHandoff("Specialist", "does the work", nil)}
	specialist.Handoffs = []*agents.Handoff{agents.NewHandoff("Root", "hands back", nil)}

	proxy := NewTemporalAgent(configs, root, streambroker.NewMemoryStreamBroker()).
		newTemporalProxyAgent(nil)

	require.NotNil(t, proxy)
	assert.NotEmpty(t, proxy.PrepareHandoffTools(context.Background()),
		"the edge out still exists; only the walk was supposed to stop")
}

// Three deep and back to the start, which is the same problem one hop further
// away and the shape a stack trace makes hardest to read.
func TestProxyAgent_HandlesALongerCycle(t *testing.T) {
	names := []string{"A", "B", "C"}
	configs := map[string]*agents.AgentOptions{}
	for _, name := range names {
		configs[name] = &agents.AgentOptions{
			Name:    name,
			History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
		}
	}
	for i, name := range names {
		next := names[(i+1)%len(names)]
		configs[name].Handoffs = []*agents.Handoff{agents.NewHandoff(next, "onwards", nil)}
	}

	assert.NotPanics(t, func() {
		proxy := NewTemporalAgent(configs, configs["A"], streambroker.NewMemoryStreamBroker()).
			newTemporalProxyAgent(nil)
		require.NotNil(t, proxy)
	})
}

// An agent that hands off to itself is the smallest cycle there is.
func TestProxyAgent_HandlesASelfHandoff(t *testing.T) {
	self := &agents.AgentOptions{
		Name:    "Self",
		History: history.NewConversationManager(history.NewInMemoryConversationPersistence()),
	}
	self.Handoffs = []*agents.Handoff{agents.NewHandoff("Self", "round again", nil)}

	assert.NotPanics(t, func() {
		proxy := NewTemporalAgent(map[string]*agents.AgentOptions{"Self": self}, self,
			streambroker.NewMemoryStreamBroker()).newTemporalProxyAgent(nil)
		require.NotNil(t, proxy)
	})
}
