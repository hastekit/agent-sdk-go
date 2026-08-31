package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubToolset is an MCP server that either lists a fixed set of tools or fails
// in a chosen way.
type stubToolset struct {
	name  string
	tools []agents.Tool
	err   error
}

func (s *stubToolset) GetName() string { return s.name }

// capturingPrompt is a SystemPromptProvider that records the dependencies it
// was handed.
type capturingPrompt func(*agents.Dependencies)

func (p capturingPrompt) GetPrompt(_ context.Context, deps *agents.Dependencies) (string, error) {
	p(deps)
	return "You are a helpful assistant.", nil
}

func (s *stubToolset) ListTools(context.Context, map[string]any) ([]agents.Tool, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.tools, nil
}

func TestPrepareMCPToolsKeepsGoingWhenAServerFails(t *testing.T) {
	healthy := &stubToolset{name: "calendar", tools: []agents.Tool{newFakeTool("list_events", false, "ok")}}
	broken := &stubToolset{name: "jira", err: errors.New("dial tcp: connection refused")}

	agent := agents.NewAgent(&agents.AgentOptions{
		Name:       "atlas",
		McpServers: []agents.MCPToolset{broken, healthy},
	})

	tools, connectors := agent.PrepareMCPTools(context.Background(), nil)

	require.Len(t, tools, 1, "the healthy server's tools must survive its neighbour failing")
	assert.Equal(t, "list_events", tools[0].GetToolDescriptor().ToolUnion.OfFunction.Name)

	// One status per configured connector, in configuration order — the run
	// needs to know what it has as much as what it is missing.
	require.Len(t, connectors, 2)

	assert.Equal(t, "jira", connectors[0].Name)
	assert.False(t, connectors[0].Connected)
	assert.Zero(t, connectors[0].ToolCount)
	assert.Equal(t, agents.ToolsetErrorUnavailable, connectors[0].Kind)
	assert.Contains(t, connectors[0].Detail, "connection refused")

	assert.Equal(t, "calendar", connectors[1].Name)
	assert.True(t, connectors[1].Connected)
	assert.Equal(t, 1, connectors[1].ToolCount)
	assert.Empty(t, connectors[1].Kind)
}

func TestPrepareMCPToolsCountsToolsPerConnector(t *testing.T) {
	agent := agents.NewAgent(&agents.AgentOptions{
		Name: "atlas",
		McpServers: []agents.MCPToolset{
			&stubToolset{name: "calendar", tools: []agents.Tool{
				newFakeTool("list_events", false, "ok"),
				newFakeTool("create_event", false, "ok"),
			}},
			&stubToolset{name: "empty"},
		},
	})

	_, connectors := agent.PrepareMCPTools(context.Background(), nil)

	require.Len(t, connectors, 2)
	assert.Equal(t, 2, connectors[0].ToolCount)
	assert.True(t, connectors[1].Connected, "a connector that listed nothing still connected")
	assert.Zero(t, connectors[1].ToolCount)
}

func TestPrepareMCPToolsClassifiesAuthFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want agents.ToolsetErrorKind
	}{
		{
			name: "typed error from the toolset",
			err:  agents.NewToolsetError(agents.ToolsetErrorAuth, errors.New("initialize: Unauthorized")),
			want: agents.ToolsetErrorAuth,
		},
		{
			// Nothing is read out of an error's text. A status that reached us
			// as prose is a status nobody classified, and the durable runtimes
			// carry the kind over in their own terms rather than leaving it to
			// be guessed at here.
			name: "an unclassified error is not sniffed for a status",
			err:  errors.New("activity error: failed to list MCP tools: initialize: Forbidden"),
			want: agents.ToolsetErrorUnavailable,
		},
		{
			name: "anything else",
			err:  errors.New("context deadline exceeded"),
			want: agents.ToolsetErrorUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := agents.NewAgent(&agents.AgentOptions{
				Name:       "atlas",
				McpServers: []agents.MCPToolset{&stubToolset{name: "calendar", err: tc.err}},
			})

			_, connectors := agent.PrepareMCPTools(context.Background(), nil)

			require.Len(t, connectors, 1)
			assert.False(t, connectors[0].Connected)
			assert.Equal(t, tc.want, connectors[0].Kind)
		})
	}
}

// The statuses reach the prompt provider as Dependencies; rendering them is
// the resolver's job, and is covered in the prompts package.
func TestConnectorStatusesReachThePromptProvider(t *testing.T) {
	var seen []agents.ConnectorStatus

	llm := &scriptedLLM{script: []*responses.Response{textResponse("I can't reach your calendar right now.")}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name: "atlas",
		Instruction: capturingPrompt(func(deps *agents.Dependencies) {
			seen = deps.Connectors
		}),
		McpServers: []agents.MCPToolset{
			&stubToolset{name: "calendar", err: agents.NewToolsetError(agents.ToolsetErrorAuth, errors.New("Unauthorized"))},
			&stubToolset{name: "notes", tools: []agents.Tool{newFakeTool("search_notes", false, "ok")}},
		},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-mcp-connectors",
		Message:   userMessage("what's on my calendar?"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	require.Len(t, seen, 2)
	assert.Equal(t, "calendar", seen[0].Name)
	assert.False(t, seen[0].Connected)
	assert.Equal(t, agents.ToolsetErrorAuth, seen[0].Kind)
	assert.Equal(t, "notes", seen[1].Name)
	assert.True(t, seen[1].Connected)
	assert.Equal(t, 1, seen[1].ToolCount)
}
