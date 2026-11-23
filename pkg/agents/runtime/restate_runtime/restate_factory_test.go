package restate_runtime

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func functionSpec(name string, background bool) agents.ToolSpec {
	return agents.ToolSpec{Tool: agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: name}}}, Background: background}
}

// The proxy is built from the journaled spec alone, cycles included, and keeps what the loop reads.
func TestFactoryProxyGraph_BuildsFromSpecs(t *testing.T) {
	root := agents.AgentRef{Name: "root", Version: "1"}
	specialist := agents.AgentRef{Name: "specialist", Version: "2"}
	specs := []agents.AgentSpec{
		{Ref: root, StickyHandoff: true, SingleTurn: true, HasInstruction: true,
			Tools:    []agents.ToolSpec{functionSpec("lookup", false), functionSpec("index", true)},
			Handoffs: []agents.HandoffSpec{{Ref: specialist, Description: "does the work"}}},
		{Ref: specialist, Handoffs: []agents.HandoffSpec{{Ref: root, Description: "hands back"}}},
	}

	// No restate context needed: nothing here runs a step.
	agent := NewFactoryAgentWorkflow(agents.AgentFactoryFunc(nil), streambroker.NewMemoryStreamBroker()).proxyGraph(nil, specs, "", "stream-1")

	require.NotNil(t, agent)
	assert.Equal(t, root, agent.Ref())
	assert.True(t, agent.StickyHandoff())
	assert.True(t, agent.SingleTurn())
	assert.NotEmpty(t, agent.PrepareHandoffTools(t.Context()))

}

func TestFactoryTool_KeepsTheBackgroundCapabilityAndDescriptor(t *testing.T) {
	plain := &factoryTool{spec: functionSpec("lookup", false)}
	var background agents.Tool = &factoryBackgroundTool{factoryTool: &factoryTool{spec: functionSpec("index", true)}}

	_, ok := background.(agents.BackgroundTool)
	assert.True(t, ok, "a tool that starts tasks stays one through the proxy")
	_, ok = agents.Tool(plain).(agents.BackgroundTool)
	assert.False(t, ok)
	assert.Equal(t, "index", agents.ToolKey(background), "the proxy describes the real tool")
}
