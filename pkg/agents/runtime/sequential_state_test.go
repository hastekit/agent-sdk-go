package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/restate_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type stateTool struct {
	*agents.BaseTool
	calls int
}

func (t *stateTool) Execute(_ context.Context, input *agents.ToolCall) (*agents.ToolCallResponse, error) {
	// Cross a serialization boundary: mutating input cannot carry the state.
	encoded, _ := json.Marshal(input)
	var call agents.ToolCall
	if err := json.Unmarshal(encoded, &call); err != nil {
		return nil, err
	}
	if t.calls > 0 && call.State["resource"] != "existing" {
		return nil, fmt.Errorf("previous update missing")
	}
	if call.State["unreturned"] != "" {
		return nil, fmt.Errorf("input mutation leaked")
	}
	if call.State == nil {
		call.State = map[string]string{}
	}
	call.State["unreturned"] = "ignored"
	t.calls++
	result := agents.ToolCallResult(&call, "ok")
	result.StateUpdates = map[string]string{"resource": "existing"}
	return result, nil
}
func TestExecutorsPropagateSequentialState(t *testing.T) {
	for name, executor := range map[string]agents.ToolExecutor{
		"local":    &agents.DefaultToolExecutor{},
		"temporal": temporal_runtime.NewTemporalToolExecutor(nil),
		"restate":  restate_runtime.NewRestateToolExecutor(nil),
	} {
		t.Run(name, func(t *testing.T) {
			tool := &stateTool{BaseTool: &agents.BaseTool{}}
			calls := make([]agents.ExecutableToolCall, 3)
			for i := range calls {
				calls[i] = agents.ExecutableToolCall{Tool: tool, ToolCall: &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{Name: "test"}}}
			}
			results := executor.ExecuteAll(context.Background(), calls)
			for _, result := range results {
				if result.Err != nil {
					t.Fatal(result.Err)
				}
			}
			if tool.calls != 3 {
				t.Fatal("missing execution")
			}
			for _, call := range calls {
				if call.ToolCall.State != nil {
					t.Fatal("caller snapshot mutated")
				}
			}
		})
	}
}
