package workflow

// Input is the run-level object a workflow invocation receives
// and accumulates into. RunContext is an opaque map the walker
// deep-merges node updates into; Status tracks per-node lifecycle
// markers; Metadata is a free-form host bag.
//
// Pause/Ports/Resumes carry the state needed to suspend a run at a
// node and resume it later. The full Input is the run's durable
// memory: serialise it when a run pauses, restore it (with a resume
// decision injected via SetResume) to continue. On resume the walker
// re-walks from the roots but short-circuits any node already marked
// completed in Status — replaying its cached Ports entry instead of
// re-executing it — so resumes are idempotent and side-effect-free
// for already-finished nodes.
type Input struct {
	RunID      string                `json:"run_id"`
	RunContext map[string]any        `json:"run_context,omitempty"`
	Status     map[string]NodeStatus `json:"status,omitempty"`
	Metadata   map[string]any        `json:"metadata,omitempty"`

	// Pause is set by the walker when a node suspends the run. Nil
	// means the run is not suspended. When set, the run stopped at
	// Pause.NodeID and is awaiting an external decision.
	Pause *PauseState `json:"pause,omitempty"`

	// Ports records the output port each completed node emitted,
	// keyed by node id. The walker uses it on resume to follow a
	// completed node's outgoing edges without re-executing it.
	Ports map[string]string `json:"ports,omitempty"`

	// Resumes carries per-node resume decisions injected by the host
	// (via SetResume) before a resume walk. A node reads its decision
	// from the resolved input the runtime hands it; see SetResume.
	Resumes map[string]map[string]any `json:"resumes,omitempty"`
}

// PauseState marks where and why a run is suspended. Payload is the
// opaque bag the pausing node supplied (see PauseError).
type PauseState struct {
	NodeID  string         `json:"node_id"`
	Payload map[string]any `json:"payload,omitempty"`
}

// NodeStatus tracks a node's lifecycle marker.
type NodeStatus string

const (
	NodeStatusRunning   NodeStatus = "running"
	NodeStatusCancelled NodeStatus = "cancelled"
	NodeStatusCompleted NodeStatus = "completed"
	NodeStatusFailed    NodeStatus = "failed"
	NodeStatusSkipped   NodeStatus = "skipped"
	NodeStatusPaused    NodeStatus = "paused"
)

// MergeContext merges update into in.RunContext. Keys whose values
// are maps on both sides merge recursively (last writer wins at
// leaves); all other values replace.
func (in *Input) MergeContext(update map[string]any) {
	if len(update) == 0 {
		return
	}
	if in.RunContext == nil {
		in.RunContext = map[string]any{}
	}
	deepMerge(in.RunContext, update)
}

// SetStatus records a node's lifecycle marker.
func (in *Input) SetStatus(nodeID string, s NodeStatus) {
	if in.Status == nil {
		in.Status = map[string]NodeStatus{}
	}
	in.Status[nodeID] = s
}

// SetPort records the output port nodeID emitted, so a resume walk
// can follow its edges without re-executing it.
func (in *Input) SetPort(nodeID, port string) {
	if in.Ports == nil {
		in.Ports = map[string]string{}
	}
	in.Ports[nodeID] = port
}

// SetResume stages a resume decision for nodeID. The host calls this
// after a pause — before re-running the workflow — to tell the
// previously suspended node how to proceed (e.g. {"decision":
// "approved"}). Setting a decision also clears any prior Pause so the
// run is no longer considered suspended.
func (in *Input) SetResume(nodeID string, decision map[string]any) {
	if in.Resumes == nil {
		in.Resumes = map[string]map[string]any{}
	}
	in.Resumes[nodeID] = decision
	in.Pause = nil
}

// Resume returns the staged resume decision for nodeID, if any.
func (in *Input) Resume(nodeID string) (map[string]any, bool) {
	if in.Resumes == nil {
		return nil, false
	}
	d, ok := in.Resumes[nodeID]
	return d, ok
}

// deepMerge copies src's keys into dst, recursing when both sides
// hold a map for the same key.
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if nested, ok := v.(map[string]any); ok {
			if existing, ok := dst[k].(map[string]any); ok {
				deepMerge(existing, nested)
				continue
			}
		}
		dst[k] = v
	}
}

// ensureInit promotes a nil *Input to an empty one and initialises
// RunContext / Status so callers don't have to pre-populate them.
func ensureInit(in *Input) *Input {
	if in == nil {
		in = &Input{}
	}
	if in.RunContext == nil {
		in.RunContext = map[string]any{}
	}
	if in.Status == nil {
		in.Status = map[string]NodeStatus{}
	}
	return in
}
