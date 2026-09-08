package workflow

import (
	"context"
	"errors"
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
