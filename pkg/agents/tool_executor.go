package agents

import (
	"context"
	"errors"
	"time"
)

// ToolExecution represents a single tool execution to be run.
type ToolExecution struct {
	ExecutableToolCall ExecutableToolCall
	Fn                 func(ctx context.Context) (*ToolCallResponse, error)
}

type ExecutableToolCall struct {
	Index    int
	ToolName string
	Tool     Tool
	ToolCall *ToolCall
}

// ToolExecutionResult holds the result of a single tool execution.
type ToolExecutionResult struct {
	Response *ToolCallResponse
	Err      error

	// Cancelled marks a call the stop unwound, as opposed to one that
	// failed on its own: the loop reports it to the model as cancelled
	// rather than as a tool error.
	//
	// Set it from the call's own outcome, never from a live read of the
	// stop flag — the outcome is already in the runtime's ledger and so
	// replays with it, while a fresh read could answer differently.
	Cancelled bool
}

// ToolExecutor executes tool calls, potentially in parallel.
// Implementations must return results in the same order as the input executions.
type ToolExecutor interface {
	ExecuteAll(ctx context.Context, executions []ExecutableToolCall) []ToolExecutionResult
}

// HookAwareToolExecutor is an optional ToolExecutor capability: the executor
// runs the agent's tool call hooks around each call. NewAgent injects the
// hooks the agent was configured with.
//
// Running them here rather than in the loop is what makes them durable steps:
// the Temporal and Restate executors run inside the workflow, so a hook they
// invoke is journaled like any other step. An executor that doesn't implement
// this simply runs no agent-level hooks.
type HookAwareToolExecutor interface {
	ToolExecutor

	// WithToolCallHooks returns a copy bound to hooks, rather than mutating,
	// so an executor shared between agents never runs another agent's hooks.
	WithToolCallHooks(hooks []ToolCallHook) ToolExecutor
}

// BrokerAwareToolExecutor is an optional ToolExecutor capability: the
// executor wants the run's broker, to watch the stop flag. NewAgent
// injects the same broker the loop uses.
type BrokerAwareToolExecutor interface {
	ToolExecutor

	// WithStreamBroker returns a copy bound to broker, rather than
	// mutating, so an executor shared between agents is never re-pointed
	// at another agent's broker.
	WithStreamBroker(broker StreamBroker) ToolExecutor
}

// DefaultToolExecutor executes tools in parallel using goroutines.
type DefaultToolExecutor struct {
	// StopWatcher is how this executor learns the run was stopped.
	// NewAgent fills it in from the broker; set it directly to override.
	// Left nil, tool calls run to completion and the stop is honoured at
	// the loop's next iteration boundary.
	StopWatcher StopWatcher

	// CancelGracePeriod bounds how long a cancelled call may take to
	// unwind before it is abandoned. Zero or less selects
	// DefaultCancelGrace.
	CancelGracePeriod time.Duration

	// Hooks wrap every call this executor runs, whatever the tool's source —
	// the agent's own function tools, its sub-agent tools, and every MCP
	// server's. This is the only place tool call hooks run.
	//
	// Handoffs do not pass through them: transfer_to_agent is settled by the
	// loop before anything reaches an executor, and the target agent's own
	// hooks apply to what it then does.
	Hooks []ToolCallHook
}

var (
	_ BrokerAwareToolExecutor = (*DefaultToolExecutor)(nil)
	_ HookAwareToolExecutor   = (*DefaultToolExecutor)(nil)
)

// WithToolCallHooks implements HookAwareToolExecutor.
func (e *DefaultToolExecutor) WithToolCallHooks(hooks []ToolCallHook) ToolExecutor {
	bound := *e
	bound.Hooks = hooks
	return &bound
}

// WithStreamBroker implements BrokerAwareToolExecutor. A watcher set by
// the caller wins; injection only fills a gap.
func (e *DefaultToolExecutor) WithStreamBroker(broker StreamBroker) ToolExecutor {
	bound := *e
	if bound.StopWatcher == nil {
		if watcher, ok := broker.(StopWatcher); ok {
			bound.StopWatcher = watcher
		}
	}
	return &bound
}

// ExecuteAll runs every call in parallel through RunStoppableTool — the
// same primitive the Temporal and Restate wrappers use, since this
// executor is where a local tool actually runs.
func (e *DefaultToolExecutor) ExecuteAll(ctx context.Context, executions []ExecutableToolCall) []ToolExecutionResult {
	results := make([]ToolExecutionResult, len(executions))

	// Per-call buffered channels rather than a shared slice: an abandoned
	// goroutine's eventual result lands in a buffer nobody reads, never in
	// memory already handed back to the caller.
	reports := make([]chan ToolExecutionResult, len(executions))
	for i, exec := range executions {
		report := make(chan ToolExecutionResult, 1)
		reports[i] = report

		go func(ex ExecutableToolCall) {
			// The hooks run inside the stoppable unit, so a stop unwinds a hook
			// that is hanging on a slow authorization service the same way it
			// unwinds a hanging tool. They sit outside the tool's own span,
			// which stays a measure of the tool.
			resp, err := RunStoppableTool(ctx, e.StopWatcher, e.CancelGracePeriod, ex.ToolCall,
				func(callCtx context.Context, params *ToolCall) (*ToolCallResponse, error) {
					return RunWithToolCallHooks(callCtx, e.Hooks, params,
						func(hookedCtx context.Context, p *ToolCall) (*ToolCallResponse, error) {
							return ExecuteWithTrace(hookedCtx, ex.Tool, p, ex.Tool.Execute)
						})
				})
			report <- ToolExecutionResult{
				Response:  resp,
				Err:       err,
				Cancelled: errors.Is(err, ErrToolCancelled),
			}
		}(exec)
	}

	for i, report := range reports {
		results[i] = <-report
	}

	return results
}
