package agents

import (
	"context"
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// WrapBackgroundTool binds the tool's agent and middleware to its AwaitTask
// operation. Execute is unchanged: its middleware belongs to the tool executor.
// Durable runtimes wrap the real tool on the worker, then call AwaitTask inside
// the wait activity or run step. The returned result is ready to journal.
func WrapBackgroundTool(agentName string, tool BackgroundTool, middlewares ...ToolCallMiddleware) BackgroundTool {
	return &backgroundToolMiddleware{BackgroundTool: tool, agentName: agentName, middlewares: middlewares}
}

type backgroundToolMiddleware struct {
	BackgroundTool
	agentName   string
	middlewares []ToolCallMiddleware
}

func (t *backgroundToolMiddleware) AwaitTask(ctx context.Context, ref BackgroundTaskRef, progress ProgressReporter) (BackgroundResult, error) {
	call := &ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{ID: ref.CallID, CallID: ref.CallID, Name: ref.ToolName},
		AgentName:           t.agentName,
		Namespace:           ref.Namespace,
		ThreadID:            ref.ThreadID,
		StreamID:            ref.TaskStreamID,
		RunContext:          ref.RunContext,
		Progress:            progress,
	}
	output, err := ExecuteWithTrace(ctx, t.BackgroundTool, call, func(ctx context.Context, call *ToolCall) (*ToolCallResponse, error) {
		return ExecuteToolCallWithMiddleware(ctx, t.middlewares, serializeTool(ExecutableToolCall{Tool: t.BackgroundTool, ToolCall: call}), call,
			func(ctx context.Context, call *ToolCall) (*ToolCallResponse, error) {
				waitRef := ref
				waitRef.RunContext = call.RunContext
				result, err := t.BackgroundTool.AwaitTask(ctx, waitRef, call.Progress)
				return &ToolCallResponse{FunctionCallOutputMessage: result.Output}, err
			})
	})
	if err != nil {
		return BackgroundResult{}, err
	}
	if output == nil {
		return BackgroundResult{}, abortedByMiddleware(fmt.Errorf("middleware returned no response for task %q", ref.TaskID))
	}
	return BackgroundResult{Output: output.FunctionCallOutputMessage}, nil
}
