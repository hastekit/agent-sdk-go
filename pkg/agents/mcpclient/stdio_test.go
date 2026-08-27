package mcpclient

import (
	"context"
	"os"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStdioServerHelper is not a test: it is the MCP server the stdio tests
// talk to, run as a child process. Re-executing the test binary is what makes a
// real server available without building one first, so these exercise the
// transport rather than a stand-in for it.
func TestStdioServerHelper(t *testing.T) {
	if os.Getenv("MCP_STDIO_HELPER") != "1" {
		t.Skip("helper process, not a test")
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "stdio-helper", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "whoami",
		Description: "Reports the value of the WHOAMI environment variable.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: os.Getenv("WHOAMI")}},
		}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}

// stdioClient builds a client that runs this test binary as its MCP server.
func stdioClient(t *testing.T, opts ...McpServerOption) *MCPClient {
	t.Helper()

	base := []McpServerOption{
		WithCommand(os.Args[0], "-test.run=TestStdioServerHelper"),
		WithEnv(map[string]string{"MCP_STDIO_HELPER": "1", "WHOAMI": "child"}),
	}
	client, err := NewClient(context.Background(), "", append(base, opts...)...)
	require.NoError(t, err)

	t.Cleanup(func() { globalPool.Remove(client.connFor(nil)) })
	return client
}

func whoamiCall() *agents.ToolCall { return echoCall("whoami") }

// A stdio server is reached by running it, and its tools work like any other's.
func TestStdioTransport_ListsAndCallsTools(t *testing.T) {
	ctx := context.Background()
	client := stdioClient(t)

	tools, err := client.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "whoami", tools[0].Tool(ctx).OfFunction.Name)

	res, err := tools[0].Execute(ctx, whoamiCall())
	require.NoError(t, err)
	require.NotNil(t, res.FunctionCallOutputMessage)
	assert.Equal(t, "child", *res.FunctionCallOutputMessage.Output.OfString,
		"the env reached the server process")
}

// Everything the http transports get applies here too: the prefix the model
// sees, and the filter written against the server's own names.
func TestStdioTransport_HonoursPrefixAndFilter(t *testing.T) {
	ctx := context.Background()
	client := stdioClient(t, WithToolPrefix("local__"), WithToolFilter("whoami"))

	tools, err := client.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "local__whoami", tools[0].Tool(ctx).OfFunction.Name)

	res, err := tools[0].Execute(ctx, echoCall("local__whoami"))
	require.NoError(t, err)
	assert.Equal(t, "child", *res.FunctionCallOutputMessage.Output.OfString)

	filtered := stdioClient(t, WithToolFilter("nothing-by-this-name"))
	tools, err = filtered.ListTools(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, tools)
}

// Env is templated from the run context, so a per-run credential reaches the
// process that needs it — the stdio counterpart of a header.
func TestStdioTransport_EnvIsResolvedFromRunContext(t *testing.T) {
	ctx := context.Background()

	client, err := NewClient(ctx, "",
		WithCommand(os.Args[0], "-test.run=TestStdioServerHelper"),
		WithEnv(map[string]string{"MCP_STDIO_HELPER": "1", "WHOAMI": "{{caller}}"}),
	)
	require.NoError(t, err)

	runContext := map[string]any{"caller": "tenant-42"}
	t.Cleanup(func() { globalPool.Remove(client.connFor(runContext)) })

	tools, err := client.ListTools(ctx, runContext)
	require.NoError(t, err)
	require.Len(t, tools, 1)

	res, err := tools[0].Execute(ctx, whoamiCall())
	require.NoError(t, err)
	assert.Equal(t, "tenant-42", *res.FunctionCallOutputMessage.Output.OfString)
}

// Two stdio servers have no endpoint to tell them apart, so the command and env
// have to be what distinguishes them — in the pool and in the schema cache.
func TestStdioServersAreDistinguishedByCommandAndEnv(t *testing.T) {
	a := serverConn{Transport: TransportStdio, Command: []string{"server-one"}}
	b := serverConn{Transport: TransportStdio, Command: []string{"server-two"}}
	sameCmdOtherEnv := serverConn{Transport: TransportStdio, Command: []string{"server-one"}, Env: map[string]string{"TOKEN": "x"}}

	assert.NotEqual(t, a.key(), b.key(), "different commands are different servers")
	assert.NotEqual(t, a.key(), sameCmdOtherEnv.key(), "same command under different credentials too")
	assert.Equal(t, a.key(), serverConn{Transport: TransportStdio, Command: []string{"server-one"}}.key())
}

// A stdio server with no command, or an http one with no endpoint, is caught
// where the message can say what is missing.
func TestConnValidation(t *testing.T) {
	err := connectErr(t, serverConn{Transport: TransportStdio})
	assert.Contains(t, err.Error(), "needs a command")

	err = connectErr(t, serverConn{Transport: TransportStreamableHTTP})
	assert.Contains(t, err.Error(), "needs an endpoint")
}

func connectErr(t *testing.T, conn serverConn) error {
	t.Helper()
	_, err := connect(context.Background(), conn)
	require.Error(t, err)
	return err
}
