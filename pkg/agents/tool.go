package agents

import (
	"context"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

type ToolCall struct {
	*responses.FunctionCallMessage
	AgentName    string            `json:"agent_name"`
	AgentVersion string            `json:"agent_version"`
	Namespace    string            `json:"namespace"`
	SessionID    string            `json:"session_id"`
	ThreadID     string            `json:"thread_id"`
	StreamID     string            `json:"stream_id,omitempty"`
	RunContext   map[string]any    `json:"run_context"`
	State        map[string]string `json:"state,omitempty"`

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
}

type Tool interface {
	Execute(ctx context.Context, params *ToolCall) (*ToolCallResponse, error)
	Tool(ctx context.Context) *responses.ToolUnion
	NeedApproval() bool
	IsDeferred() bool
}

type BaseTool struct {
	ToolUnion        responses.ToolUnion
	RequiresApproval bool
	Deferred         bool

	// Annotations are the tool's self-reported behavioural hints (read-only,
	// destructive, ...). Nil when the tool declared none. They ride along with
	// the rest of BaseTool across a durable runtime's serialization boundary,
	// so a policy on the far side sees the same hints the server sent.
	Annotations *ToolAnnotations
}

func (t *BaseTool) NeedApproval() bool {
	return t.RequiresApproval
}

func (t *BaseTool) GetAnnotations() *ToolAnnotations {
	return t.Annotations
}

func (t *BaseTool) IsDeferred() bool {
	return t.Deferred
}

func (t *BaseTool) Tool(ctx context.Context) *responses.ToolUnion {
	return &t.ToolUnion
}

// partitionByApproval splits tool calls into those needing approval and those that can execute immediately
func partitionByApproval(ctx context.Context, tools []Tool, toolCalls []responses.FunctionCallMessage) (needsApproval []responses.FunctionCallMessage, immediate []responses.FunctionCallMessage) {
	for _, toolCall := range toolCalls {
		tool := findTool(ctx, tools, toolCall.Name)
		if tool != nil && tool.NeedApproval() {
			needsApproval = append(needsApproval, toolCall)
		} else {
			immediate = append(immediate, toolCall)
		}
	}
	return needsApproval, immediate
}

// findTool finds a tool by name
func findTool(ctx context.Context, tools []Tool, toolName string) Tool {
	for _, tool := range tools {
		if t := tool.Tool(ctx); t != nil && t.OfFunction != nil && t.OfFunction.Name == toolName {
			return tool
		}
	}
	return nil
}
