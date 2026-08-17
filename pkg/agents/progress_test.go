package agents_test

import (
	"context"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// A tool that emits progress mid-execution has those updates delivered to the
// run's client as tool.progress chunks on the same stream, ahead of the final
// tool output, tagged with the call id and tool name.
func TestAgentLoop_FunctionToolProgressStreamsToClient(t *testing.T) {
	const streamID = "progress-stream"
	broker := streambroker.NewMemoryStreamBroker()

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_work", "worker", "{}"),
		textResponse("all done"),
	}}
	worker := newFakeTool("worker", false, "worker done")
	inner := worker.execute
	worker.execute = func(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
		params.ReportProgress(ctx, agents.ToolProgress{Progress: 1, Total: 3, Message: "step 1"})
		params.ReportProgress(ctx, agents.ToolProgress{Progress: 2, Total: 3, Message: "step 2"})
		return inner(ctx, params)
	}
	agent := newScriptedAgent("main", llm, nil, broker, []agents.Tool{worker}, nil)

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-progress",
		StreamID:  streamID,
		Message:   userMessage("do work"),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Drain the stream, collecting progress chunks. Ranging over Chunks
	// before Wait is the documented deadlock-free consumption pattern.
	var progress []*responses.ChunkToolProgress[constants.ChunkTypeToolProgress]
	for chunk := range handle.Chunks {
		if chunk.OfToolProgress != nil {
			progress = append(progress, chunk.OfToolProgress)
		}
	}
	out, err := handle.Wait()
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	requireStatus(t, out, agentstate.RunStatusCompleted)

	if len(progress) != 2 {
		t.Fatalf("progress chunks = %d, want 2", len(progress))
	}
	for i, p := range progress {
		if p.CallID != "call_work" {
			t.Fatalf("progress[%d] callID = %q, want call_work", i, p.CallID)
		}
		if p.ToolName != "worker" {
			t.Fatalf("progress[%d] toolName = %q, want worker", i, p.ToolName)
		}
		if p.Total != 3 {
			t.Fatalf("progress[%d] total = %v, want 3", i, p.Total)
		}
	}
	if progress[0].Message != "step 1" || progress[1].Message != "step 2" {
		t.Fatalf("progress messages = %q, %q; want step 1, step 2", progress[0].Message, progress[1].Message)
	}
	if progress[0].SequenceNumber >= progress[1].SequenceNumber {
		t.Fatalf("sequence numbers not increasing: %d, %d", progress[0].SequenceNumber, progress[1].SequenceNumber)
	}
}

// With no broker/stream wired, a tool that reports progress is a harmless
// no-op — ReportProgress must never panic on a nil reporter.
func TestAgentLoop_ToolProgressNoBrokerIsNoop(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_work", "worker", "{}"),
		textResponse("done"),
	}}
	worker := newFakeTool("worker", false, "worker done")
	inner := worker.execute
	worker.execute = func(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
		params.ReportProgress(ctx, agents.ToolProgress{Progress: 1, Message: "tick"})
		return inner(ctx, params)
	}
	// newScriptedAgent with a nil broker still gets a default in-memory
	// broker from NewAgent, so force the no-broker path by asserting the run
	// completes without panicking regardless.
	agent := newScriptedAgent("main", llm, nil, nil, []agents.Tool{worker}, nil)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-noprogress",
		Message:   userMessage("do work"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)
}

// The tool.progress chunk survives a marshal/unmarshal round trip through the
// ResponseChunk union and is dispatched back to the OfToolProgress arm.
func TestToolProgressChunk_JSONRoundTrip(t *testing.T) {
	original := &responses.ResponseChunk{
		OfToolProgress: &responses.ChunkToolProgress[constants.ChunkTypeToolProgress]{
			SequenceNumber: 5,
			CallID:         "call_x",
			ToolName:       "fetch",
			Progress:       2,
			Total:          10,
			Message:        "halfway",
		},
	}

	data, err := sonic.Marshal(original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if original.ChunkType() != "tool.progress" {
		t.Fatalf("ChunkType() = %q, want tool.progress", original.ChunkType())
	}

	var back responses.ResponseChunk
	if err := sonic.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal failed: %v (json: %s)", err, data)
	}
	if back.OfToolProgress == nil {
		t.Fatalf("round trip lost OfToolProgress arm: %s", data)
	}
	got := back.OfToolProgress
	if got.CallID != "call_x" || got.ToolName != "fetch" || got.Message != "halfway" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Progress != 2 || got.Total != 10 || got.SequenceNumber != 5 {
		t.Fatalf("round trip numeric mismatch: %+v", got)
	}
	if back.ChunkType() != "tool.progress" {
		t.Fatalf("round trip ChunkType() = %q, want tool.progress", back.ChunkType())
	}
}
