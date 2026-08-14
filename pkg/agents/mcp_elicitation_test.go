package agents_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bookSeatServer mirrors samples/mcpserver: a tool that asks for a form
// before it can answer, served over streamable-http.
func bookSeatServer(t *testing.T) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "sample", Version: "0.1.0"}, nil)
	type bookSeatIn struct {
		FlightNo string `json:"flight_no"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "book_seat", Description: "reserve a seat"},
		func(_ context.Context, req *mcp.CallToolRequest, _ bookSeatIn) (*mcp.CallToolResult, any, error) {
			if answer, ok := req.Params.InputResponses["passenger_details"]; ok {
				name, _ := answer.(*mcp.ElicitResult).Content["full_name"].(string)
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "Seat reserved for " + name}},
				}, nil, nil
			}
			return &mcp.CallToolResult{
				InputRequests: mcp.InputRequestMap{
					"passenger_details": &mcp.ElicitParams{
						Mode:    "form",
						Message: "Passenger details, as printed on the passport.",
						RequestedSchema: map[string]any{
							"type":       "object",
							"properties": map[string]any{"full_name": map[string]any{"type": "string"}},
							"required":   []string{"full_name"},
						},
					},
				},
				RequestState: "TP1234",
			}, nil, nil
		})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(hs.Close)
	return hs.URL
}

// The reported failure, end to end: an agent calls an MCP tool that needs
// input from the user. The run must pause with the form rather than hand the
// model "mcpclient: elicitation required" as the tool's answer.
func TestAgentLoop_MCPElicitationPausesRun(t *testing.T) {
	endpoint := bookSeatServer(t)

	toolset, err := mcpclient.NewClient(context.Background(), endpoint,
		mcpclient.WithTransport("streamable-http"))
	require.NoError(t, err)

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_book", "book_seat", `{"flight_no":"TP1234"}`),
		textResponse("all set"),
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:       "atlas",
		McpServers: []agents.MCPToolset{toolset},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-mcp-elicit",
		Message:   userMessage("book me a seat on TP1234"),
	})

	requireStatus(t, out, agentstate.RunStatusPaused)
	require.Len(t, out.Interrupts, 1, "the MCP server's input request must pause the run")

	intr := out.Interrupts[0]
	assert.Equal(t, responses.InterruptModeForm, intr.Mode)
	assert.Equal(t, "call_book", intr.FunctionCallMessage.CallID)
	require.Len(t, intr.Elicitations, 1)
	assert.Equal(t, "Passenger details, as printed on the passport.", intr.Elicitations[0].Message)

	// Submit the form; the run resumes and the tool completes.
	out = runAgent(t, agent, &agents.AgentInput{
		Namespace:     "test",
		ThreadID:      "thread-mcp-elicit",
		PreviousRunID: out.RunID,
		Message:       elicitationMessage("call_book", `{"full_name":"Ada Lovelace"}`),
	})

	requireStatus(t, out, agentstate.RunStatusCompleted)
	assert.Contains(t, messagesText(out.Output), "Seat reserved for Ada Lovelace")
}
