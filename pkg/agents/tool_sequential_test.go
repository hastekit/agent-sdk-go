package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type sequentialTool struct {
	*BaseTool
	started chan int
	gates   []chan struct{}
}

func (t *sequentialTool) Execute(ctx context.Context, call *ToolCall) (*ToolCallResponse, error) {
	id := call.RunContext["id"].(int)
	t.started <- id
	select {
	case <-t.gates[id]:
		return ToolCallResult(call, call.CallID), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestExecutorRunsAllToolsSequentially(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tool := &sequentialTool{BaseTool: &BaseTool{}, started: make(chan int, 6), gates: make([]chan struct{}, 6)}
	calls := make([]ExecutableToolCall, 6)
	for i := range calls {
		tool.gates[i] = make(chan struct{})
		calls[i] = ExecutableToolCall{Tool: tool, ToolName: "test", ToolCall: &ToolCall{
			FunctionCallMessage: &responses.FunctionCallMessage{Name: "test", CallID: string(rune('a' + i))},
			RunContext:          map[string]any{"id": i},
		}}
	}
	done := make(chan []ToolExecutionResult, 1)
	go func() { done <- (&DefaultToolExecutor{}).ExecuteAll(ctx, calls) }()
	next := func() int {
		t.Helper()
		select {
		case id := <-tool.started:
			return id
		case <-ctx.Done():
			t.Fatal("executor did not start expected call")
			return -1
		}
	}
	for id := range calls {
		if got := next(); got != id {
			t.Fatalf("call %d overtaken by %d", id, got)
		}
		select {
		case got := <-tool.started:
			t.Fatalf("call %d overlapped call %d", got, id)
		default:
		}
		close(tool.gates[id])
	}
	select {
	case results := <-done:
		for i, result := range results {
			if result.Err != nil {
				t.Fatal(result.Err)
			}
			if result.Response.CallID != calls[i].ToolCall.CallID {
				t.Fatal("results reordered")
			}
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

type sequentialFailureTool struct {
	*BaseTool
	calls   int
	failure error
}

func (t *sequentialFailureTool) Execute(_ context.Context, call *ToolCall) (*ToolCallResponse, error) {
	t.calls++
	if t.calls == 1 {
		return nil, t.failure
	}
	return ToolCallResult(call, "ok"), nil
}
func TestSequentialExecutorStopsAfterCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		failure := errors.New("command failed")
		if cancelled {
			failure = ErrToolCancelled
		}
		tool := &sequentialFailureTool{BaseTool: &BaseTool{}, failure: failure}
		calls := make([]ExecutableToolCall, 3)
		for i := range calls {
			calls[i] = ExecutableToolCall{Tool: tool, ToolCall: &ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{Name: "test"}}}
		}
		results := (&DefaultToolExecutor{}).ExecuteAll(context.Background(), calls)
		if !errors.Is(results[0].Err, failure) {
			t.Fatal("first error lost")
		}
		if cancelled {
			if tool.calls != 1 {
				t.Fatal("started another call after cancellation")
			}
			for _, result := range results {
				if !result.Cancelled {
					t.Fatal("remaining call not marked cancelled")
				}
			}
		} else if tool.calls != 3 {
			t.Fatal("ordinary tool error prevented later calls")
		}
	}
}
