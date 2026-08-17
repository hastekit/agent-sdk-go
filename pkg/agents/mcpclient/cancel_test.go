package mcpclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type blockInput struct{}

// Cancelling has to reach the MCP server, where the work actually is.
// This runs a real server over the same streamable-http transport, calls
// a tool that blocks on its context, and asserts the handler sees the
// cancellation.
func TestCallToolDirect_CancellationReachesServer(t *testing.T) {
	started := make(chan struct{})
	serverSawCancel := make(chan struct{})

	server := mcp.NewServer(&mcp.Implementation{Name: "block-server", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "block",
		Description: "Blocks until its context is cancelled.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in blockInput) (*mcp.CallToolResult, any, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(serverSawCancel)
			return nil, nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "ran to completion"}},
			}, nil, nil
		}
	})

	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, nil,
	))
	// The pool keeps its session on context.Background() by design, so a
	// plain Close would block forever waiting on that connection.
	defer func() {
		httpSrv.CloseClientConnections()
		httpSrv.Close()
	}()

	client, err := mcpclient.NewClient(context.Background(), httpSrv.URL,
		mcpclient.WithTransport("streamable-http"))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	callDone := make(chan error, 1)
	go func() {
		_, err := client.CallToolDirect(ctx, nil, &agents.ToolCall{
			FunctionCallMessage: &responses.FunctionCallMessage{
				ID:        "fc_block",
				CallID:    "call_block",
				Name:      "block",
				Arguments: "{}",
			},
		})
		callDone <- err
	}()

	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("tool never started on the server")
	}

	cancel()

	select {
	case <-serverSawCancel:
	case <-time.After(20 * time.Second):
		t.Fatal("the MCP server kept running the tool: the cancellation never reached it")
	}

	select {
	case err := <-callDone:
		require.ErrorIs(t, err, context.Canceled, "a cancelled call should report the cancellation")
	case <-time.After(20 * time.Second):
		t.Fatal("the client never returned from the cancelled call")
	}
}
