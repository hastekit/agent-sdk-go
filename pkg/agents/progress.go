package agents

import (
	"context"
	"sync/atomic"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// ToolProgress is a single mid-execution progress update emitted by a tool.
// Its fields mirror MCP's notifications/progress so MCP tools map their
// server-sent progress onto it verbatim, while function tools construct it
// directly. Progress should increase monotonically across a call; Total is
// optional (0 means unknown).
type ToolProgress struct {
	Progress float64
	Total    float64
	Message  string
}

// ProgressReporter lets a running tool emit progress updates that stream to
// the run's client on the same channel as the rest of the run. It is injected
// into ToolCall.Progress at execution time (never serialized), so it is a
// runtime-agnostic seam: the local runtime publishes straight to the broker,
// and durable runtimes re-inject their own reporter inside the tool activity.
//
// Report is best-effort and must be safe to call concurrently and on a nil
// reporter is guarded by ToolCall.ReportProgress — tool authors should prefer
// that helper over calling Report directly.
type ProgressReporter interface {
	Report(ctx context.Context, update ToolProgress)
}

// ReportProgress emits a progress update for this call if a reporter is
// wired. It is nil-safe, so tools can call it unconditionally without
// worrying about which runtime injected (or didn't inject) a reporter.
func (t *ToolCall) ReportProgress(ctx context.Context, update ToolProgress) {
	if t == nil || t.Progress == nil {
		return
	}
	t.Progress.Report(ctx, update)
}

// brokerProgressReporter publishes tool progress as tool.progress chunks on
// the run's stream. Progress is a live side stream: it is published directly
// (never through DurableStep) because it is inherently non-deterministic and
// must not be journaled or replayed. SequenceNumber is monotonic per call.
type brokerProgressReporter struct {
	broker   StreamBroker
	streamID string
	callID   string
	toolName string
	seq      atomic.Int64
}

func (r *brokerProgressReporter) Report(ctx context.Context, update ToolProgress) {
	if r == nil || r.broker == nil || r.streamID == "" {
		return
	}
	_ = r.broker.Publish(ctx, r.streamID, &responses.ResponseChunk{
		OfToolProgress: &responses.ChunkToolProgress[constants.ChunkTypeToolProgress]{
			SequenceNumber: int(r.seq.Add(1)),
			CallID:         r.callID,
			ToolName:       r.toolName,
			Progress:       update.Progress,
			Total:          update.Total,
			Message:        update.Message,
		},
	})
}

// NewStreamProgressReporter builds a ProgressReporter that publishes
// tool.progress chunks to the given broker/stream. It exists for runtimes that
// must re-establish the reporter after a ToolCall crosses a serialization
// boundary that drops ToolCall.Progress — e.g. the Temporal tool activity,
// which rebuilds it from the worker's broker and the workflow execution id.
// It returns nil when broker or streamID is empty, which is nil-safe via
// ToolCall.ReportProgress and the MCP bridge.
func NewStreamProgressReporter(broker StreamBroker, streamID, callID, toolName string) ProgressReporter {
	if broker == nil || streamID == "" {
		return nil
	}
	return &brokerProgressReporter{
		broker:   broker,
		streamID: streamID,
		callID:   callID,
		toolName: toolName,
	}
}

// progressReporter builds a reporter bound to one tool call on the given
// stream. It returns nil when the agent has no broker or no stream, which is
// nil-safe through ToolCall.ReportProgress and the MCP bridge.
func (e *Agent) progressReporter(streamID, callID, toolName string) ProgressReporter {
	return NewStreamProgressReporter(e.streamBroker, streamID, callID, toolName)
}
