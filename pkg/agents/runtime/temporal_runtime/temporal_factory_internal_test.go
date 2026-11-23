package temporal_runtime

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The workflow builds its proxy from the journaled spec alone, cycles included.
func TestFactoryProxyGraph_BuildsFromSpecs(t *testing.T) {
	root := agents.AgentRef{Name: "root", Version: "1"}
	specialist := agents.AgentRef{Name: "specialist", Version: "2"}
	specs := []agents.AgentSpec{
		{Ref: root, StickyHandoff: true, SingleTurn: true, Handoffs: []agents.HandoffSpec{{Ref: specialist, Description: "does the work"}}},
		{Ref: specialist, Handoffs: []agents.HandoffSpec{{Ref: root, Description: "hands back"}, {Ref: specialist, Description: "self"}}},
	}

	// No workflow context needed: the proxies only store it.
	agent := NewFactoryAgentWorkflow(agents.AgentFactoryFunc(nil), streambroker.NewMemoryStreamBroker()).proxyGraph(nil, specs)

	require.NotNil(t, agent)
	assert.Equal(t, root, agent.Ref())
	assert.True(t, agent.StickyHandoff())
	assert.True(t, agent.SingleTurn())
	assert.NotEmpty(t, agent.PrepareHandoffTools(t.Context()))
}

func TestFactoryToolProxy_KeepsTheBackgroundCapabilityAndRoute(t *testing.T) {
	ref := agents.AgentRef{Name: "an3", Version: "1"}
	spec := agents.ToolSpec{Tool: agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "index"}}}, Background: true}

	tool := newFactoryToolProxy(nil, ref, spec)
	proxy, ok := tool.(*TemporalBackgroundToolProxy)
	require.True(t, ok, "a tool that starts tasks stays one through the proxy")
	assert.Equal(t, "index", agents.ToolKey(tool))
	assert.Equal(t, ref, *proxy.ref)
	assert.Equal(t, "index", proxy.toolKey)

	plain := newFactoryToolProxy(nil, ref, agents.ToolSpec{Tool: spec.Tool})
	_, ok = plain.(agents.BackgroundTool)
	assert.False(t, ok)
}
