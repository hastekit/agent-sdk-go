package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type waitMiddleware func(agents.ToolCallFunc) agents.ToolCallFunc

func (h waitMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc { return h(next) }

type middlewareWaitTool struct {
	*agents.BaseTool
	wait func(context.Context, agents.BackgroundTaskRef) (agents.BackgroundResult, error)
}

func (t *middlewareWaitTool) Execute(context.Context, *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{TaskID: "job"}, nil
}

func (t *middlewareWaitTool) AwaitTask(ctx context.Context, ref agents.BackgroundTaskRef, _ agents.ProgressReporter) (agents.BackgroundResult, error) {
	return t.wait(ctx, ref)
}

func TestBackgroundMiddlewareWrapsTheWait(t *testing.T) {
	for _, shortCircuit := range []bool{false, true} {
		t.Run(map[bool]string{false: "invoke", true: "short circuit"}[shortCircuit], func(t *testing.T) {
			var order []string
			tool := &middlewareWaitTool{
				BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: "render"}}},
				wait: func(ctx context.Context, ref agents.BackgroundTaskRef) (agents.BackgroundResult, error) {
					order = append(order, "wait")
					require.Equal(t, "updated", ref.RunContext["value"])
					return agents.BackgroundResult{Output: agents.BackgroundText("finished")}, nil
				},
			}
			middleware := waitMiddleware(func(next agents.ToolCallFunc) agents.ToolCallFunc {
				return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
					order = append(order, "before")
					require.Equal(t, "specialist", call.AgentName)
					require.Equal(t, "tenant", call.Namespace)
					require.Equal(t, "render", tool.Name)
					if shortCircuit {
						return agents.ToolCallResult(call, "cached"), nil
					}
					copy := *call
					copy.RunContext = map[string]any{"value": "updated"}
					result, err := next(ctx, tool, &copy)
					order = append(order, "after")
					return result, err
				}
			})
			bound := agents.WrapBackgroundTool("specialist", tool, middleware)
			ref := agents.BackgroundTaskRef{AgentName: "owner", Namespace: "tenant", ToolName: "render", RunContext: map[string]any{"value": "original"}}
			result, err := bound.AwaitTask(t.Context(), ref, nil)
			require.NoError(t, err)
			require.Equal(t, "original", ref.RunContext["value"])
			if shortCircuit {
				require.Equal(t, []string{"before"}, order)
				require.Equal(t, "cached", *result.Output.Output.OfString)
			} else {
				require.Equal(t, []string{"before", "wait", "after"}, order)
				require.Equal(t, "finished", *result.Output.Output.OfString)
			}
		})
	}
}

func TestBackgroundMiddlewareCanRecoverWaitFailure(t *testing.T) {
	failure := errors.New("wait failed")
	tool := &middlewareWaitTool{BaseTool: &agents.BaseTool{}, wait: func(context.Context, agents.BackgroundTaskRef) (agents.BackgroundResult, error) {
		return agents.BackgroundResult{}, failure
	}}
	middleware := waitMiddleware(func(next agents.ToolCallFunc) agents.ToolCallFunc {
		return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			_, err := next(ctx, tool, call)
			require.ErrorIs(t, err, failure)
			return agents.ToolCallResult(call, "recovered"), nil
		}
	})
	result, err := agents.WrapBackgroundTool("agent", tool, middleware).AwaitTask(t.Context(), agents.BackgroundTaskRef{ToolName: "render"}, nil)
	require.NoError(t, err)
	require.Equal(t, "recovered", *result.Output.Output.OfString)
}
