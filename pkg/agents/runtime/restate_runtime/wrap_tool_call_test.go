package restate_runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type transformMediaTool struct{ *agents.BaseTool }

func (t *transformMediaTool) AwaitTask(ctx context.Context, _ agents.BackgroundTaskRef, _ agents.ProgressReporter) (agents.BackgroundResult, error) {
	result, err := t.Execute(ctx, nil)
	return agents.BackgroundResult{Output: result.FunctionCallOutputMessage}, err
}

func (*transformMediaTool) Execute(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
	uri := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII="
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{CallID: "call", Output: responses.FunctionCallOutputContentUnion{OfList: responses.InputContent{{OfInputImage: &responses.InputImageContent{ImageURL: &uri}}}}}}, nil
}

type transformMediaToolset struct{ tool agents.Tool }

func (*transformMediaToolset) GetName() string { return "media" }
func (t *transformMediaToolset) ListTools(context.Context, map[string]any) ([]agents.Tool, error) {
	return []agents.Tool{t.tool}, nil
}

func TestRestateRunBodiesReturnReferences(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})
	tool := &transformMediaTool{BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "media"}}}}
	ctx := t.Context()
	call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{CallID: "call", Name: "media"}, Namespace: "tenant"}
	// A call with no namespace has nowhere to store, and the run ends rather
	// than letting the bytes through.
	homeless := *call
	homeless.Namespace = ""
	local := NewRestateTool(nil, tool, nil, middleware)
	mcp := NewRestateMCPTool(nil, &transformMediaToolset{tool: tool}, nil, *tool.BaseTool, nil, middleware)
	for _, execute := range []func(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error){local.execute, mcp.execute} {
		result, err := execute(ctx, call)
		require.NoError(t, err)
		bytes, err := json.Marshal(result)
		require.NoError(t, err)
		require.Contains(t, string(bytes), "attachment://")
		require.NotContains(t, string(bytes), "base64")
		ref, err := attachments.RefFromFileID(*result.Output.OfList[0].OfInputImage.FileID)
		require.NoError(t, err)
		_, err = store.Lookup(ctx, "tenant", ref)
		require.NoError(t, err)
		_, err = execute(ctx, &homeless)
		require.Error(t, err)
		require.True(t, wasAborted(err))
	}
}

func TestBackgroundRegistryBindsMiddlewareBeforeSerialization(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	tool := &transformMediaTool{BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "media"}}}}
	configs := map[string]*agents.AgentOptions{
		"owner":      {Name: "owner", Tools: []agents.Tool{tool}},
		"specialist": {Name: "specialist", Tools: []agents.Tool{tool}, Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})}},
	}
	service := NewBackgroundTaskService(configs, nil)
	for _, name := range []string{"owner", "specialist"} {
		proxy := newRestateTool(nil, name, tool, nil).(*RestateBackgroundTool)
		bound, err := service.backgroundTool(&BackgroundTaskInput{AgentName: "owner", ToolKey: proxy.key})
		require.NoError(t, err)
		result, err := bound.AwaitTask(t.Context(), agents.BackgroundTaskRef{AgentName: "owner", ToolName: "media", Namespace: "tenant"}, nil)
		require.NoError(t, err)
		wire, err := json.Marshal(result)
		require.NoError(t, err)
		if name == "specialist" {
			require.Contains(t, string(wire), "attachment://")
			require.NotContains(t, string(wire), "base64")
		} else {
			require.Contains(t, string(wire), "base64", "middleware remains optional")
		}
	}
}
