// This example builds and resumes a workflow entirely in Go.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/workflow"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	// Start a real MCP server locally so the demo needs no external services.
	// In an application, connect to your MCP server's URL instead.
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "payments", Version: "1.0.0"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "record_payment", Description: "Record an approved payment (demo)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in paymentInput) (*mcp.CallToolResult, any, error) {
			message := fmt.Sprintf("Recorded approved payment of %.2f %s", in.Amount, in.Currency)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: message}}}, nil, nil
		})
	mcpHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true},
	))
	defer mcpHTTP.Close()
	connector, err := mcpclient.NewClient(ctx, "payments", mcpHTTP.URL,
		mcpclient.WithTransport(mcpclient.TransportStreamableHTTP))
	if err != nil {
		return err
	}

	prepare, err := workflow.NewJavaScriptNode("prepare", workflow.JavaScriptNodeConfig{
		Code: "return {amount: input.amount, currency: 'USD'};",
	})
	if err != nil {
		return err
	}
	review, err := workflow.NewHumanNode("review", workflow.HumanNodeConfig{
		Message: "${{ 'Approve payment of ' + nodes.prepare.amount + ' USD?' }}",
	})
	if err != nil {
		return err
	}
	accepted, err := workflow.NewJavaScriptNode("accepted", workflow.JavaScriptNodeConfig{
		Code: "return {approved: true, payment: nodes.prepare};",
	})
	if err != nil {
		return err
	}
	record, err := workflow.NewMCPNode("record", workflow.MCPNodeConfig{
		Server: connector,
		Tool:   "record_payment",
		Arguments: map[string]any{
			"amount":   "${{ nodes.accepted.payment.amount }}",
			"currency": "${{ nodes.accepted.payment.currency }}",
		},
	})
	if err != nil {
		return err
	}
	compiled, err := workflow.NewGraph("review-payment").
		AddNode("prepare", prepare).
		AddNode("review", review).
		AddNode("accepted", accepted).
		AddNode("record", record).
		AddEdge(workflow.StartNode, "prepare").
		AddEdge("prepare", "review").
		AddEdgeOnPort("review", "approved", "accepted").
		AddEdgeOnPort("review", "rejected", workflow.EndNode).
		AddEdge("accepted", "record").
		AddEdge("record", workflow.EndNode).
		Compile()
	if err != nil {
		return err
	}
	state, err := compiled.Execute(ctx, &workflow.Input{
		RunContext: map[string]any{"input": map[string]any{"amount": 150}},
	})
	if err != nil {
		return err
	}
	if state.Pause != nil {
		fmt.Println("Paused for human review:", state.Pause.NodeID)
		// Demo decision. In an application, persist state and wait for the user.
		state.SetResume(state.Pause.NodeID, map[string]any{"action": "approve"})
		state, err = compiled.Execute(ctx, state)
		if err != nil {
			return err
		}
	}
	output, err := json.MarshalIndent(state.RunContext["nodes"], "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(output))
	return nil
}

type paymentInput struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}
