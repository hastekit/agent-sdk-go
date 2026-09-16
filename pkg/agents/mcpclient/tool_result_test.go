package mcpclient

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestMCPMediaResultReachesAttachmentMiddleware(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII=")
	require.NoError(t, err)
	call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{ID: "id", CallID: "call"}, Namespace: "test"}
	result, err := mcpToolResult(call, []mcp.Content{
		&mcp.TextContent{Text: "Image and document:"},
		&mcp.ImageContent{MIMEType: "image/png", Data: png},
		&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///reports/report.pdf", MIMEType: "application/pdf", Blob: []byte("%PDF-1.7\nexample\n%%EOF")}},
		&mcp.TextContent{Text: "End"},
	})
	require.NoError(t, err)
	require.Len(t, result.Output.OfList, 4)
	require.Equal(t, "End", result.Output.OfList[3].OfInputText.Text)
	require.Equal(t, "report.pdf", *result.Output.OfList[2].OfInputFile.FileName)
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})
	out, err := middleware.WrapToolCall(func(context.Context, *agents.BaseTool, *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return result, nil
	})(t.Context(), nil, call)
	require.NoError(t, err)
	require.NotSame(t, result, out)
	require.NotNil(t, out.Output.OfList[1].OfInputImage.FileID)
	require.NotNil(t, out.Output.OfList[2].OfInputFile.FileID)
	plain, err := mcpToolResult(call, []mcp.Content{&mcp.TextContent{Text: "plain"}})
	require.NoError(t, err)
	require.Equal(t, "plain", *plain.Output.OfString)
	_, err = mcpToolResult(call, nil)
	require.Error(t, err)
}
