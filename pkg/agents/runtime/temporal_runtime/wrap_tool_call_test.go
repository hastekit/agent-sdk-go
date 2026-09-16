package temporal_runtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

func rawAttachment() *agents.ToolCallResponse {
	uri := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII="
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{ID: "id", CallID: "call", Output: responses.FunctionCallOutputContentUnion{OfList: responses.InputContent{{OfInputImage: &responses.InputImageContent{ImageURL: &uri}}}}}}
}

type attachmentActivityTool struct {
	*agents.BaseTool
	calls int
}

func (t *attachmentActivityTool) Execute(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
	t.calls++
	return rawAttachment(), nil
}
func (t *attachmentActivityTool) AwaitTask(context.Context, agents.BackgroundTaskRef, agents.ProgressReporter) (agents.BackgroundResult, error) {
	t.calls++
	return agents.BackgroundResult{Output: rawAttachment().FunctionCallOutputMessage}, nil
}

type activityTransform struct {
	*agentmiddleware.AttachmentMiddleware
	t *testing.T
}

func (h *activityTransform) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	wrapped := h.AttachmentMiddleware.WrapToolCall(next)
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		require.True(h.t, activity.IsActivity(ctx), "the wrap must run in an activity, not workflow code")
		return wrapped(ctx, tool, call)
	}
}

type transformToolset struct{ tool agents.Tool }

func (*transformToolset) GetName() string { return "media_server" }
func (s *transformToolset) ListTools(context.Context, map[string]any) ([]agents.Tool, error) {
	return []agents.Tool{s.tool}, nil
}

// attachmentProducer answers every call with inline media without running the
// tool: the case the attachment middleware has to be outermost for.
type attachmentProducer struct {
	agents.NoopMiddleware
}

func (*attachmentProducer) WrapToolCall(agents.ToolCallFunc) agents.ToolCallFunc {
	return func(context.Context, *agents.BaseTool, *agents.ToolCall) (*agents.ToolCallResponse, error) {
		return rawAttachment(), nil
	}
}

func TestTemporalResultsAreTransformedBeforeActivitySerialization(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	middleware := &activityTransform{AttachmentMiddleware: agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store}), t: t}
	tool := &attachmentActivityTool{BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "media"}}}}
	// The attachment middleware first, so it is outermost and sees what the
	// producer inside it answers with.
	middlewares := []agents.Middleware{middleware, &attachmentProducer{}}
	a := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{Name: "A", History: history.NewConversationManager(history.NewInMemoryConversationPersistence()), Tools: []agents.Tool{tool}, McpServers: []agents.MCPToolset{&transformToolset{tool: tool}}, Middlewares: middlewares}, nil)
	activities := a.GetActivities()
	for name := range activities {
		require.NotContains(t, name, "WrapToolCall", "wraps must not become separate activities")
	}
	for _, name := range []string{"A_media_ExecuteToolActivity", "A_media_server_ExecuteMCPToolActivity", "A_media_AwaitTaskActivity"} {
		t.Run(name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			fn, ok := activities[name]
			require.True(t, ok, name)
			env.RegisterActivity(fn)
			// Every activity stores under the namespace the call carries.
			call := &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{ID: "id", CallID: "call", Name: "media"}, Namespace: "test"}
			var args []interface{}
			switch name {
			case "A_media_ExecuteToolActivity":
				args = []interface{}{call}
			case "A_media_server_ExecuteMCPToolActivity":
				args = []interface{}{tool.BaseTool, call, map[string]any{}}
			case "A_media_AwaitTaskActivity":
				args = []interface{}{agents.BackgroundTaskRef{CallID: "call", ToolName: "media", Namespace: "test"}}
			}
			value, err := env.ExecuteActivity(fn, args...)
			require.NoError(t, err)
			var wire map[string]any
			require.NoError(t, value.Get(&wire))
			b, err := json.Marshal(wire)
			require.NoError(t, err)
			require.Contains(t, string(b), "attachment://")
			require.NotContains(t, string(b), "base64")
			require.NotContains(t, string(b), "image_url")
		})
	}
	require.Zero(t, tool.calls, "short-circuit middleware must skip Execute and AwaitTask")
}

func TestSharedMCPServerBindsEachAgentsMiddleware(t *testing.T) {
	tool := newMiddlewareTestTool("search")
	server := &transformToolset{tool: tool}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	for _, name := range []string{"owner", "specialist"} {
		a := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
			Name: name, History: newTestHistory(), McpServers: []agents.MCPToolset{server},
			Middlewares: []agents.Middleware{&authzMiddleware{name: name}},
		}, nil)
		activityName := name + "_media_server_ExecuteMCPToolActivity"
		fn := a.GetActivities()[activityName]
		require.NotNil(t, fn)
		env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: activityName})
	}
	for _, name := range []string{"owner", "specialist"} {
		call := middlewareCall()
		// Policy belongs to the registered activity, not a caller-controlled name.
		call.AgentName = "unrelated"
		value, err := env.ExecuteActivity(name+"_media_server_ExecuteMCPToolActivity", tool.BaseTool, call, map[string]any{})
		require.NoError(t, err)
		var result agents.ToolCallResponse
		require.NoError(t, value.Get(&result))
		require.Equal(t, "denied by "+name, *result.Output.OfString)
	}
	require.False(t, tool.ran)
}
