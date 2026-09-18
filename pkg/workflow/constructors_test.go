package workflow_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/workflow"
	"github.com/stretchr/testify/require"
)

type nodeAgent struct{}

func (nodeAgent) Run(ctx context.Context, in *agents.AgentInput) (*agents.AgentOutput, error) {
	return &agents.AgentOutput{RunID: "child", Status: agentstate.RunStatusCompleted}, ctx.Err()
}

type nodeTool struct{ agents.BaseTool }

func (t *nodeTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	text := call.Arguments
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{CallID: call.CallID, Output: responses.FunctionCallOutputContentUnion{OfString: &text}}}, ctx.Err()
}

type nodeServer struct{ tool agents.Tool }

func (nodeServer) GetName() string { return "demo" }
func (s nodeServer) ListTools(context.Context, map[string]any) ([]agents.Tool, error) {
	return []agents.Tool{s.tool}, nil
}
func mustNode(t *testing.T, node workflow.Node, err error) workflow.Node {
	t.Helper()
	require.NoError(t, err)
	return node
}

func TestProgrammaticNodesMatchYAML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Thu, 01 Jan 1970 00:00:00 GMT")
		_, _ = w.Write([]byte(`{"amount":3}`))
	}))
	defer server.Close()
	connector := nodeServer{&nodeTool{agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "record"}}}}}
	agent := nodeAgent{}
	graph := workflow.NewGraph("typed")
	n, err := workflow.NewAPINode("api", workflow.APINodeConfig{URL: server.URL})
	graph.AddNode("api", mustNode(t, n, err))
	n, err = workflow.NewJavaScriptNode("code", workflow.JavaScriptNodeConfig{Code: "return nodes.api.body.amount * 2;"})
	graph.AddNode("code", mustNode(t, n, err))
	n, err = workflow.NewIfElseNode("if", workflow.IfElseNodeConfig{Condition: "nodes.code === 6"})
	graph.AddNode("if", mustNode(t, n, err))
	n, err = workflow.NewSwitchNode("switch", workflow.SwitchNodeConfig{Value: "${{ nodes.code }}", Cases: []workflow.SwitchCase{{Value: 6, Port: "six"}}})
	graph.AddNode("switch", mustNode(t, n, err))
	n, err = workflow.NewDelayNode("delay", workflow.DelayNodeConfig{Duration: 0 * time.Second})
	graph.AddNode("delay", mustNode(t, n, err))
	n, err = workflow.NewHumanNode("human", workflow.HumanNodeConfig{Message: "Proceed?"})
	graph.AddNode("human", mustNode(t, n, err))
	n, err = workflow.NewAgentNode("agent", workflow.AgentNodeConfig{Agent: agent, Message: "${{ 'Amount: ' + nodes.code }}"})
	graph.AddNode("agent", mustNode(t, n, err))
	n, err = workflow.NewMCPNode("mcp", workflow.MCPNodeConfig{Server: connector, Tool: "record", Arguments: map[string]string{"amount": "${{ nodes.code }}"}})
	graph.AddNode("mcp", mustNode(t, n, err))
	graph.AddEdge("START", "api").AddEdge("api", "code").AddEdge("code", "if").AddEdgeOnPort("if", "true", "switch").AddEdgeOnPort("if", "false", "END").AddEdgeOnPort("switch", "six", "delay").AddEdge("switch", "END").AddEdge("delay", "human").AddEdgeOnPort("human", "approved", "agent").AddEdgeOnPort("human", "rejected", "END").AddEdge("agent", "mcp").AddEdge("mcp", "END")
	typed, err := graph.Compile()
	require.NoError(t, err)
	yaml, err := workflow.LoadYAML([]byte(fmt.Sprintf(`version: 1
id: yaml
nodes:
 - {id: api, type: api, config: {url: '%s'}}
 - {id: code, type: javascript, config: {code: 'return nodes.api.body.amount * 2;'}}
 - {id: if, type: if_else, config: {condition: 'nodes.code === 6'}}
 - {id: switch, type: switch, config: {value: '${{ nodes.code }}', cases: [{value: 6, port: six}]}}
 - {id: delay, type: delay, config: {duration: 0s}}
 - {id: human, type: human, config: {message: 'Proceed?'}}
 - {id: agent, type: agent, config: {agent: helper, message: "${{ 'Amount: ' + nodes.code }}"}}
 - {id: mcp, type: mcp, config: {server: demo, tool: record, arguments: {amount: '${{ nodes.code }}'}}}
edges:
 - {from: START, to: api}
 - {from: api, to: code}
 - {from: code, to: if}
 - {from: if, port: 'true', to: switch}
 - {from: if, port: 'false', to: END}
 - {from: switch, port: six, to: delay}
 - {from: switch, to: END}
 - {from: delay, to: human}
 - {from: human, port: approved, to: agent}
 - {from: human, port: rejected, to: END}
 - {from: agent, to: mcp}
 - {from: mcp, to: END}
`, server.URL)), workflow.Dependencies{Agents: map[string]workflow.AgentRunner{"helper": agent}, MCPServers: map[string]agents.MCPToolset{"demo": connector}})
	require.NoError(t, err)
	states := []*workflow.Input{}
	for _, compiled := range []*workflow.Compiled{typed, yaml} {
		state, err := compiled.Execute(context.Background(), &workflow.Input{RunID: "same-run"})
		require.NoError(t, err)
		require.NotNil(t, state.Pause)
		require.Equal(t, "human", state.Pause.NodeID)
		state.SetResume("human", map[string]any{"action": "approve"})
		state, err = compiled.Execute(context.Background(), state)
		require.NoError(t, err)
		require.Nil(t, state.Pause)
		states = append(states, state)
	}
	require.Equal(t, states[0].RunContext, states[1].RunContext)
	require.Equal(t, states[0].Status, states[1].Status)
}

func TestProgrammaticNodeValidationAndSnapshots(t *testing.T) {
	_, err := workflow.NewHumanNode("", workflow.HumanNodeConfig{Message: "Approve"})
	require.Error(t, err)
	_, err = workflow.NewAPINode("api", workflow.APINodeConfig{URL: "${{ input.url"})
	require.Error(t, err)
	_, err = workflow.NewDelayNode("delay", workflow.DelayNodeConfig{Duration: -time.Second})
	require.Error(t, err)
	_, err = workflow.NewMCPNode("mcp", workflow.MCPNodeConfig{Tool: "missing"})
	require.Error(t, err)
	_, err = workflow.NewAgentNode("agent", workflow.AgentNodeConfig{Message: "missing"})
	require.Error(t, err)
	_, err = workflow.NewJavaScriptNode("js", workflow.JavaScriptNodeConfig{Code: "return 1;", Timeout: -time.Second})
	require.Error(t, err)
	n, err := workflow.NewHumanNode("human", workflow.HumanNodeConfig{Message: "Approve"})
	require.NoError(t, err)
	_, err = workflow.NewGraph("bad-id").AddNode("other", n).Compile()
	require.ErrorContains(t, err, "does not match graph key")
	_, err = workflow.NewGraph("bad-port").AddNode("human", n).AddEdge("human", "END").Compile()
	require.ErrorContains(t, err, "no port")
	cases := []workflow.SwitchCase{{Value: 1, Port: "one"}}
	n, err = workflow.NewSwitchNode("switch", workflow.SwitchNodeConfig{Value: 1, Cases: cases})
	require.NoError(t, err)
	cases[0].Value = 2
	cases[0].Port = "changed"
	compiled, err := workflow.NewGraph("copy").AddNode("switch", n).AddEdgeOnPort("switch", "one", "END").Compile()
	require.NoError(t, err)
	result, err := compiled.Execute(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, "one", result.Ports["switch"])
}
