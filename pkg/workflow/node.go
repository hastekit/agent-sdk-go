package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// NodeType identifies the kind of node in a workflow.
type NodeType string

// PauseError is the sentinel a node's Execute returns to suspend the
// run instead of completing or failing. The walker catches it,
// records the node as paused, and returns the run as suspended (not
// failed) so an external decision can resume it later. Payload is an
// opaque, host-defined bag describing what the run is waiting on
// (e.g. an approval request's title and description); it is surfaced
// on Input.Pause for the caller to act on.
type PauseError struct {
	Payload map[string]any
}

func (e *PauseError) Error() string {
	return "workflow: node paused awaiting external input"
}

// Pause builds a *PauseError carrying payload. Nodes call this from
// Execute to suspend the run.
func Pause(payload map[string]any) error {
	return &PauseError{Payload: payload}
}

// IsPauseErr reports whether err is (or wraps) a *PauseError, returning
// it when so.
func IsPauseErr(err error) (*PauseError, bool) {
	var pe *PauseError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// Node is the interface every workflow node implements. Validate
// runs at Compile time; Execute runs at run time and returns a
// partial RunContext update plus the port name edges should follow.
// Execute must honor ctx cancellation, pass ctx to blocking operations, and
// return promptly once cancelled. It must not mutate Input: return updates
// through output instead. Parallel nodes share the input for the current wave.
type Node interface {
	Type() NodeType
	Validate() error
	Execute(ctx context.Context, in *Input) (output map[string]any, port string, err error)
}

// BaseNode is a convenience embedding that supplies Type().
type BaseNode struct {
	NodeType NodeType
}

// Type returns the node's kind.
func (b *BaseNode) Type() NodeType { return b.NodeType }

// NodeFactory builds a Node. Host-side dependencies are captured
// in the closure.
type NodeFactory func() (Node, error)

type builtinNode struct {
	BaseNode
	id      string
	ports   map[string]bool
	delay   *time.Duration // built-in delays can use a durable runtime timer
	execute func(context.Context, *Input) (any, string, error)
}

func (n *builtinNode) Validate() error { return nil }
func (n *builtinNode) Execute(ctx context.Context, in *Input) (map[string]any, string, error) {
	// Keep suspended integrations dormant until their own decision arrives.
	if saved := in.Suspended[n.id]; saved != nil {
		if _, ok := in.Resume(n.id); !ok {
			return nil, "", Pause(saved.Payload)
		}
	}
	result, port, err := n.execute(ctx, in)
	if err != nil {
		return nil, "", err
	}
	// Outputs must survive history serialization and cannot alias shared input.
	result, err = jsonValue(result)
	if err != nil {
		return nil, "", fmt.Errorf("node output must be JSON: %w", err)
	}
	return map[string]any{"nodes": map[string]any{n.id: result}}, port, nil
}
