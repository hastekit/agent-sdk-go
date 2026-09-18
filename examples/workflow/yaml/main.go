package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hastekit/agent-sdk-go/pkg/workflow"
)

//go:embed review.yaml
var descriptor []byte

func main() {
	listen := flag.String("listen", "", "serve independent workflow HTTP endpoints at this address")
	flag.Parse()
	if err := run(*listen); err != nil {
		log.Fatal(err)
	}
}

func run(listen string) error {
	ctx := context.Background()
	// Local demo services keep the example runnable without credentials. Replace
	// these endpoints with your API and MCP server in an application.
	policyAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"reviewLimit": 100})
	}))
	defer policyAPI.Close()

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "approvals", Version: "1.0.0"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "record_approval", Description: "Record an approved request (demo)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in approvalInput) (*mcp.CallToolResult, any, error) {
			// A real implementation would persist the approval here.
			message := fmt.Sprintf("Recorded approval for %.2f: %s", in.Amount, in.Note)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: message}}}, nil, nil
		})
	mcpHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true},
	))
	defer mcpHTTP.Close()
	connector, err := mcpclient.NewClient(ctx, "approvals", mcpHTTP.URL,
		mcpclient.WithTransport(mcpclient.TransportStreamableHTTP))
	if err != nil {
		return err
	}

	compiled, err := workflow.LoadYAML(descriptor, workflow.Dependencies{
		MCPServers: map[string]agents.MCPToolset{"approvals": connector},
	})
	if err != nil {
		return err
	}
	registry := hastekit.NewRegistry()
	if err := registry.RegisterWorkflow("review", compiled); err != nil {
		return err
	}
	if listen != "" {
		handler := hastekit.NewWorkflowHTTPHandler(registry, workflow.HTTPConfig{
			RunContextResolver: func(*http.Request) (map[string]any, error) {
				return map[string]any{"policy_url": policyAPI.URL}, nil
			},
		})
		log.Printf("Workflow API listening at http://%s/workflows", listen)
		server := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		return server.ListenAndServe()
	}
	state, err := registry.RunWorkflow(ctx, "review", &workflow.Input{
		RunContext: map[string]any{"input": map[string]any{"amount": 150}, "context": map[string]any{"policy_url": policyAPI.URL}},
		Metadata:   map[string]any{"namespace": "demo"},
	})
	if err != nil {
		return err
	}
	if state.Pause != nil {
		// Persist state and collect the user's decision in a real application.
		// Here the example supplies a decision directly.
		state.SetResume(state.Pause.NodeID, map[string]any{
			"action": "approve", "content": map[string]any{"note": "Reviewed"},
		})
		state, err = registry.RunWorkflow(ctx, "review", state)
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

type approvalInput struct {
	Amount float64 `json:"amount"`
	Note   string  `json:"note"`
}
