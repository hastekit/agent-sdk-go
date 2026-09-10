package agents

import (
	"context"
	"encoding/json"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type ToolCall struct {
	*responses.FunctionCallMessage
	AgentName    string         `json:"agent_name"`
	AgentVersion string         `json:"agent_version"`
	Namespace    string         `json:"namespace"`
	SessionID    string         `json:"session_id"`
	ThreadID     string         `json:"thread_id"`
	StreamID     string         `json:"stream_id,omitempty"`
	RunContext   map[string]any `json:"run_context"`

	// State is the run's key-value scratchpad: whatever earlier tools and
	// middlewares have written, and whatever survived from earlier runs on this
	// thread.
	//
	// Write with ToolCallResponse.StateUpdates, not by assigning here. A
	// durable runtime rebuilds this map from a serialized payload, so writes
	// to it on the far side reach nothing; it is copied locally too, so that
	// mistake fails the same way in both places instead of only in production.
	State map[string]string `json:"state,omitempty"`

	// ShouldResume tells Execute to continue an in-flight call
	// instead of starting a fresh one. The tool implementation reads
	// this flag and switches to its resume code path — typically
	// recovering saved per-call state from State (e.g., a sub-agent's
	// thread id and prior run id) and feeding ResumeMessages into
	// the underlying run as a continuation.
	ShouldResume bool `json:"should_resume,omitempty"`

	// ResumeMessages carries the continuation messages the tool
	// should forward into its inner run on resume — typically a
	// single FunctionCallInterruptResolutionMessage built by the agent
	// loop from QueuedApprovals / QueuedRejections. Nil when
	// ShouldResume is false.
	ResumeMessages []responses.InputMessageUnion `json:"resume_messages,omitempty"`

	// Progress is the sink for mid-execution progress updates. It is
	// injected by whoever runs the tool in-process (the agent loop for the
	// local runtime; the tool activity/step for durable runtimes) and is
	// deliberately not serialized — it does not survive an activity
	// boundary and is re-injected on the far side. Tools should emit via
	// ReportProgress, which is nil-safe, rather than touching this directly.
	Progress ProgressReporter `json:"-"`
}

type ToolCallResponse struct {
	*responses.FunctionCallOutputMessage
	StateUpdates map[string]string     `json:"state_updates,omitempty"`
	Interrupts   []responses.Interrupt `json:"interrupts,omitempty"`

	// TaskID says the tool has started work that outlives this call, and
	// names it. The Output alongside it is what the model reads now — "started
	// indexing, job 41ff" — and the run carries on rather than waiting.
	//
	// The tool must implement BackgroundTool: the agent calls AwaitTask to
	// wait for the outcome, and delivers it to the thread when it arrives. A
	// task id from a tool that cannot be waited on fails the run, because the
	// work has already started and nothing would ever report it.
	TaskID string `json:"task_id,omitempty"`

	// TaskPayload is whatever the tool needs when it is asked to wait, carried
	// back to it on BackgroundTaskRef.Payload.
	//
	// It exists because starting a task and waiting for one are not the same
	// call, and under a durable runtime they are not even the same process:
	// anything the wait needs — the call's arguments, a cursor, a handle — has
	// to travel, and a field in memory does not.
	TaskPayload json.RawMessage `json:"task_payload,omitempty"`
}

type Tool interface {
	Execute(ctx context.Context, params *ToolCall) (*ToolCallResponse, error)

	// GetToolDescriptor projects the tool onto the plain data every tool has in
	// common — its schema, its name, and the flags the loop reads off it. That
	// projection is the only form of the tool that can cross a durable
	// runtime's boundary or reach a middleware, and it is also what the loop itself
	// reads, so a tool describes itself in exactly one place. Embedding
	// *BaseTool satisfies this.
	//
	// Describing itself is not something a tool is allowed to fail at: nil is
	// the only way to say nothing, and every caller treats a tool that says
	// nothing as one that is not there.
	GetToolDescriptor() *BaseTool
}

type BaseTool struct {
	// Name is the tool's own name, without any prefix the model-facing name
	// carries. Only sources that have a name of their own set it — an MCP
	// server's tools do, a locally defined function tool does not, since for it
	// the two are the same name. A middleware is always shown it filled in either way
	// (see serializeTool).
	Name             string
	ToolUnion        responses.ToolUnion
	RequiresApproval bool
	Deferred         bool

	// Annotations are the tool's self-reported behavioural hints (read-only,
	// destructive, ...). Nil when the tool declared none. They ride along with
	// the rest of BaseTool across a durable runtime's serialization boundary,
	// so a policy on the far side sees the same hints the server sent.
	Annotations *ToolAnnotations
	Meta        map[string]any
}

// GetToolDescriptor implements Tool. A tool that embeds *BaseTool is already
// the plain data, so this hands back the embedded value itself.
func (t *BaseTool) GetToolDescriptor() *BaseTool {
	return t
}

// toolDescriptor is GetToolDescriptor for the call sites that hold a tool they
// may not have found — a nil tool describes itself as nothing, same as a tool
// that has nothing to say.
func toolDescriptor(tool Tool) *BaseTool {
	if tool == nil {
		return nil
	}
	return tool.GetToolDescriptor()
}

// functionName is the model-facing name of a tool, or "" for one that is not a
// function tool (a provider-side web search, say) or does not describe itself.
func functionName(tool Tool) string {
	descriptor := toolDescriptor(tool)
	if descriptor == nil || descriptor.ToolUnion.OfFunction == nil {
		return ""
	}
	return descriptor.ToolUnion.OfFunction.Name
}

// partitionByApproval splits tool calls into those needing approval and those that can execute immediately
func partitionByApproval(tools []Tool, toolCalls []responses.FunctionCallMessage) (needsApproval []responses.FunctionCallMessage, immediate []responses.FunctionCallMessage) {
	for _, toolCall := range toolCalls {
		tool := findTool(tools, toolCall.Name)
		if descriptor := toolDescriptor(tool); descriptor != nil && descriptor.RequiresApproval {
			needsApproval = append(needsApproval, toolCall)
		} else {
			immediate = append(immediate, toolCall)
		}
	}
	return needsApproval, immediate
}

// findTool finds a tool by name
func findTool(tools []Tool, toolName string) Tool {
	for _, tool := range tools {
		if name := functionName(tool); name != "" && name == toolName {
			return tool
		}
	}
	return nil
}
