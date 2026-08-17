package temporal_runtime_test

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"go.temporal.io/sdk/testsuite"
)

// progressTool reports one progress update mid-execution, then returns a
// normal text output.
type progressTool struct {
	*agents.BaseTool
}

func (t *progressTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	params.ReportProgress(ctx, agents.ToolProgress{Progress: 1, Total: 2, Message: "halfway"})
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("worker done")},
		},
	}, nil
}

// The tool activity runs the tool with ToolCall.Progress stripped by the
// workflow→activity serialization boundary; NewTemporalTool must re-inject a
// broker-backed reporter so the tool's progress still reaches the run stream.
func TestTemporalToolActivity_ReinjectsProgressReporter(t *testing.T) {
	// The activity test environment reports this fixed workflow execution id,
	// which is the stream channel the injected reporter publishes on.
	const streamID = "default-test-workflow-id"

	broker := streambroker.NewMemoryStreamBroker()
	chunks, err := broker.Subscribe(context.Background(), streamID)
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	tool := &progressTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "worker",
					Description: utils.Ptr("test tool"),
					Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
	}
	temporalTool := temporal_runtime.NewTemporalTool(tool, broker)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(temporalTool.Execute)

	val, err := env.ExecuteActivity(temporalTool.Execute, &agents.ToolCall{
		FunctionCallMessage: &responses.FunctionCallMessage{
			ID:     "fc_1",
			CallID: "call_1",
			Name:   "worker",
		},
	})
	if err != nil {
		t.Fatalf("activity failed: %v", err)
	}

	var out agents.ToolCallResponse
	if err := val.Get(&out); err != nil {
		t.Fatalf("decode activity result: %v", err)
	}
	if out.FunctionCallOutputMessage == nil || out.Output.OfString == nil || *out.Output.OfString != "worker done" {
		t.Fatalf("unexpected tool output: %+v", out.FunctionCallOutputMessage)
	}

	// The progress update the tool emitted must have been published to the
	// stream by the reporter the activity re-injected.
	broker.Close(context.Background(), streamID)
	var found *responses.ResponseChunk
	for chunk := range chunks {
		if chunk.OfToolProgress != nil {
			found = chunk
			break
		}
	}
	if found == nil {
		t.Fatalf("no tool.progress chunk published to the stream")
	}
	p := found.OfToolProgress
	if p.CallID != "call_1" || p.ToolName != "worker" {
		t.Fatalf("progress chunk mislabeled: callID=%q toolName=%q", p.CallID, p.ToolName)
	}
	if p.Message != "halfway" || p.Progress != 1 || p.Total != 2 {
		t.Fatalf("progress chunk payload wrong: %+v", p)
	}
}
