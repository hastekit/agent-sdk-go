package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Runtime executes a compiled workflow. Implementations must
// return the *Input even on error so callers can inspect partial
// state.
type Runtime interface {
	Execute(ctx context.Context, c *Compiled, in *Input, opts RuntimeOptions) (*Input, error)
}

// RuntimeOptions carries run-level configuration into a Runtime
// call. A zero value picks defaults.
type RuntimeOptions struct {
	Logger   *slog.Logger
	MaxSteps int
}

type invokeConfig struct {
	runtime     Runtime
	runtimeOpts RuntimeOptions
}

// InvokeOption configures a single Invoke call.
type InvokeOption func(*invokeConfig)

// WithRuntime selects the Runtime used for the invocation.
// Defaults to InProcessRuntime when unset.
func WithRuntime(rt Runtime) InvokeOption {
	return func(c *invokeConfig) { c.runtime = rt }
}

// WithLogger overrides the slog logger used during the run.
func WithLogger(l *slog.Logger) InvokeOption {
	return func(c *invokeConfig) { c.runtimeOpts.Logger = l }
}

// WithMaxSteps caps how many node executions a single run may
// perform.
func WithMaxSteps(n int) InvokeOption {
	return func(c *invokeConfig) { c.runtimeOpts.MaxSteps = n }
}

// ConditionalEdge attaches a runtime router to a node. Router
// returns a label the walker resolves against Targets to pick the
// next node.
type ConditionalEdge struct {
	Router  Router
	Targets map[string]string
}

// Compiled is a validated, prepared graph ready for execution.
// Read-only to Runtimes; mutable execution state lives on Input.
type Compiled struct {
	Nodes        map[string]Node
	OutEdges     map[string][]Edge
	Conditionals map[string]ConditionalEdge
	Roots        []string
}

// Execute runs the compiled graph. The Runtime defaults to
// InProcessRuntime when WithRuntime is not supplied.
func (c *Compiled) Execute(ctx context.Context, in *Input, opts ...InvokeOption) (*Input, error) {
	cfg := invokeConfig{runtime: InProcessRuntime{}}
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.runtime == nil {
		return nil, fmt.Errorf("workflow: Execute given a nil Runtime via WithRuntime")
	}

	return cfg.runtime.Execute(ctx, c, in, cfg.runtimeOpts)
}

// InProcessRuntime runs graphs through the shared Walker using a
// goroutine-per-node executor. Cancellation is cooperative: it cancels running
// nodes and waits for them to unwind before returning their shared Input.
// Nodes that ignore ctx can delay cancellation; goroutines cannot be killed.
type InProcessRuntime struct{}

const defaultMaxSteps = 500

// Execute implements Runtime by delegating to the shared Walker
// with a goroutine-per-node executor.
func (InProcessRuntime) Execute(ctx context.Context, c *Compiled, in *Input, opts RuntimeOptions) (*Input, error) {
	w := NewWalker(opts)
	return w.Walk(ctx, c, in, inProcessExecutor{logger: w.Logger})
}

// inProcessExecutor runs each invocation in its own goroutine. The
// first failing node cancels peers via the shared wave context.
type inProcessExecutor struct {
	logger *slog.Logger
}

func (e inProcessExecutor) ExecuteWave(ctx context.Context, invs []Invocation) []Result {
	results := make([]Result, len(invs))
	waveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(len(invs))
	for i, inv := range invs {
		go func(i int, inv Invocation) {
			defer wg.Done()
			results[i] = runInvocation(waveCtx, inv, cancel)
		}(i, inv)
	}
	wg.Wait()
	return results
}

// runInvocation calls the node's Execute with the shared Input.
// The walker guarantees no concurrent writes to RunContext during a
// wave, so reads here are race-free.
func runInvocation(ctx context.Context, inv Invocation, onFail context.CancelFunc) Result {
	if err := cancellationError(ctx); err != nil {
		return Result{NodeID: inv.NodeID, Err: err}
	}
	output, port, err := inv.Node.Execute(ctx, inv.Input)
	// Discard updates from work that returned after its context was cancelled.
	// Cancellation also takes precedence over a pause from that work.
	if cancelErr := cancellationError(ctx); cancelErr != nil {
		return Result{NodeID: inv.NodeID, Err: errors.Join(err, cancelErr)}
	}
	if err != nil {
		// A pause is a suspension, not a failure: surface it as a
		// Result.Pause and do not cancel sibling nodes in the wave.
		if pe, ok := IsPauseErr(err); ok {
			return Result{NodeID: inv.NodeID, Pause: &PauseState{NodeID: inv.NodeID, Payload: pe.Payload}}
		}
		onFail()
		return Result{NodeID: inv.NodeID, Err: fmt.Errorf("node %q failed: %w", inv.NodeID, err)}
	}
	return Result{NodeID: inv.NodeID, Output: output, Port: port}
}

// Keep standard errors.Is checks working while preserving WithCancelCause and
// WithDeadlineCause details supplied by the caller.
func cancellationError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if cause := context.Cause(ctx); cause != err {
			return errors.Join(err, cause)
		}
		return err
	}
	return nil
}
