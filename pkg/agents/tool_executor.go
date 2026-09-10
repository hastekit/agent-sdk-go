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

// MiddlewareAwareToolExecutor is an optional ToolExecutor capability: the executor
// runs the agent's tool call middlewares around each call. NewAgent injects the
// middlewares the agent was configured with.
//
// The local executor runs them directly. Durable executors leave them on the
// worker, inside the same step as the tool. An executor that does not implement
// this capability must arrange middleware at its own execution boundary.
type MiddlewareAwareToolExecutor interface {
	ToolExecutor

	// WithToolCallMiddlewares returns a copy bound to middlewares, rather than mutating,
	// so an executor shared between agents never runs another agent's middleware.
	WithToolCallMiddlewares(middlewares []ToolCallMiddleware) ToolExecutor
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

	// Middlewares wrap every call this executor runs, whatever the tool's source —
	// the agent's own function tools, its sub-agent tools, and every MCP
	// server's. This is the only place tool call middlewares run.
	//
	// Handoffs do not pass through them: transfer_to_agent is settled by the
	// loop before anything reaches an executor, and the target agent's own
	// middlewares apply to what it then does.
	Middlewares []ToolCallMiddleware
}

var (
	_ BrokerAwareToolExecutor     = (*DefaultToolExecutor)(nil)
	_ MiddlewareAwareToolExecutor = (*DefaultToolExecutor)(nil)
)

// WithToolCallMiddlewares implements MiddlewareAwareToolExecutor.
func (e *DefaultToolExecutor) WithToolCallMiddlewares(middlewares []ToolCallMiddleware) ToolExecutor {
	bound := *e
	bound.Middlewares = middlewares
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

// ExecuteAll runs calls in parallel, with stop middleware outside user middleware.
func (e *DefaultToolExecutor) ExecuteAll(ctx context.Context, executions []ExecutableToolCall) []ToolExecutionResult {
	results := make([]ToolExecutionResult, len(executions))
	middlewares := append([]ToolCallMiddleware{StopMiddleware{Watcher: e.StopWatcher, CancelGracePeriod: e.CancelGracePeriod}}, e.Middlewares...)

	// Per-call buffered channels rather than a shared slice: an abandoned
	// goroutine's eventual result lands in a buffer nobody reads, never in
	// memory already handed back to the caller.
	reports := make([]chan ToolExecutionResult, len(executions))
	for i, exec := range executions {
		report := make(chan ToolExecutionResult, 1)
		reports[i] = report

		go func(ex ExecutableToolCall) {
			resp, err := ExecuteWithTrace(ctx, ex.Tool, ex.ToolCall, func(ctx context.Context, call *ToolCall) (*ToolCallResponse, error) {
				return ExecuteToolWithMiddleware(ctx, middlewares, ex, ex.Tool.Execute)
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
