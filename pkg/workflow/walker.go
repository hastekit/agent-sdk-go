package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
)

// NodeExecutor is the seam between the walker and a specific
// runtime. It must return one Result per Invocation in order and
// honour ctx cancellation.
type NodeExecutor interface {
	ExecuteWave(ctx context.Context, invocations []Invocation) []Result
}

// Invocation is one unit of work the walker sends to the executor.
type Invocation struct {
	Node   Node
	NodeID string
	Input  *Input
}

// Result is the outcome of one node execution. Output is the
// node's partial RunContext update.
type Result struct {
	NodeID string
	Output map[string]any
	Port   string
	Err    error

	// Pause, when non-nil, means the node suspended the run instead of
	// completing. The walker records the node as paused, stamps
	// Input.Pause, and stops the walk. Executors translate a node's
	// PauseError into this field.
	Pause *PauseState
}

// Walker drives wave-based graph execution: dedup, result merging,
// cycle-free visitation, and step-cap enforcement. The NodeExecutor
// plugs in per-runtime dispatch.
type Walker struct {
	Logger   *slog.Logger
	MaxSteps int
}

// NewWalker constructs a Walker using opts, filling in defaults
// for unset fields.
func NewWalker(opts RuntimeOptions) *Walker {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxSteps := opts.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	return &Walker{Logger: logger, MaxSteps: maxSteps}
}

// Walk executes the compiled graph through ne. It always returns
// the non-nil *Input, even on error, so observers can inspect
// partial state. Each Result.Output is deep-merged into
// in.RunContext; Status is recorded per node.
func (w *Walker) Walk(ctx context.Context, c *Compiled, in *Input, ne NodeExecutor) (out *Input, err error) {
	in = ensureInit(in)
	defer func() {
		if cancelErr := cancellationError(ctx); cancelErr != nil {
			if !errors.Is(err, ctx.Err()) || !errors.Is(err, context.Cause(ctx)) {
				err = errors.Join(err, cancelErr)
			}
			in.Pause = nil
			for id := range c.Nodes {
				switch in.Status[id] {
				case NodeStatusRunning, NodeStatusPaused:
					in.SetStatus(id, NodeStatusCancelled)
				case NodeStatusCompleted, NodeStatusFailed, NodeStatusCancelled:
					// Preserve settled work for inspection or a later retry.
				default:
					in.SetStatus(id, NodeStatusSkipped)
				}
			}
		}
	}()
	if err := cancellationError(ctx); err != nil {
		return in, err
	}
	// A fresh walk is not suspended; any prior Pause is either being
	// resumed (cleared by SetResume) or is stale from a serialised
	// Input. Either way, start clean and let this walk re-derive it.
	in.Pause = nil

	steps := 0
	wave := append([]string(nil), c.Roots...)
	visited := make(map[string]bool, len(c.Nodes))

	for len(wave) > 0 {
		if err := cancellationError(ctx); err != nil {
			return in, err
		}
		unique := dedupUnvisited(wave, visited)
		if len(unique) == 0 {
			break
		}

		// Stable dispatch order so deterministic runtimes (Temporal)
		// replay identically.
		sort.Strings(unique)
		for _, id := range unique {
			visited[id] = true
		}

		// On a resume walk, nodes already completed in a prior run are
		// short-circuited: we replay their cached port to follow edges
		// instead of re-executing them (idempotent, side-effect-free).
		// Only nodes that still need to run are dispatched to the
		// executor.
		var invs []Invocation
		results := make([]Result, 0, len(unique))
		for _, id := range unique {
			if err := cancellationError(ctx); err != nil {
				return in, err
			}
			if in.Status[id] == NodeStatusCompleted {
				port, ok := in.Ports[id]
				if !ok {
					port = DefaultPort
				}
				w.Logger.Info("short-circuiting completed node", "node_id", id, "port", port)
				results = append(results, Result{NodeID: id, Port: port})
				continue
			}
			node := c.Nodes[id]
			in.SetStatus(id, NodeStatusRunning)
			invs = append(invs, Invocation{Node: node, NodeID: id, Input: in})
			steps++
			w.Logger.Info("dispatching node", "node_id", id, "type", node.Type())
		}

		if steps > w.MaxSteps {
			return in, fmt.Errorf("workflow: exceeded max steps (%d)", w.MaxSteps)
		}

		if len(invs) > 0 {
			results = append(results, ne.ExecuteWave(ctx, invs)...)
		}

		var waveErrs []error
		var nextCandidates []string
		var paused bool
		for _, r := range results {
			if r.Pause != nil && ctx.Err() == nil {
				in.SetStatus(r.NodeID, NodeStatusPaused)
				in.Pause = r.Pause
				if in.Pause.NodeID == "" {
					in.Pause.NodeID = r.NodeID
				}
				w.Logger.Info("node paused awaiting external input", "node_id", r.NodeID)
				paused = true
				continue
			}
			if r.Err != nil {
				if errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
					in.SetStatus(r.NodeID, NodeStatusCancelled)
					w.Logger.Info("node execution cancelled", "node_id", r.NodeID, "error", r.Err)
				} else {
					in.SetStatus(r.NodeID, NodeStatusFailed)
					w.Logger.Error("node execution failed", "node_id", r.NodeID, "error", r.Err)
				}
				waveErrs = append(waveErrs, fmt.Errorf("node %q: %w", r.NodeID, r.Err))
				continue
			}
			if r.Pause != nil { // Cancellation supersedes a pending pause.
				in.SetStatus(r.NodeID, NodeStatusCancelled)
				continue
			}
			in.MergeContext(r.Output)
			in.SetStatus(r.NodeID, NodeStatusCompleted)
			in.SetPort(r.NodeID, r.Port)

			if ctx.Err() != nil {
				continue
			}

			// Conditional edges take precedence over static port
			// edges.
			if cond, ok := c.Conditionals[r.NodeID]; ok {
				label := cond.Router(in)
				target, mapped := cond.Targets[label]
				if !mapped {
					w.Logger.Info("conditional edge: unmapped label, branch ends",
						"node_id", r.NodeID, "label", label)
					continue
				}
				if target == EndNode {
					continue
				}
				if !visited[target] {
					nextCandidates = append(nextCandidates, target)
				}
				continue
			}

			key := edgeKey(r.NodeID, r.Port)
			for _, edge := range c.OutEdges[key] {
				if edge.ToNode == EndNode {
					continue
				}
				if !visited[edge.ToNode] {
					nextCandidates = append(nextCandidates, edge.ToNode)
				}
			}
		}
		if cancelErr := cancellationError(ctx); cancelErr != nil {
			return in, errors.Join(append(waveErrs, cancelErr)...)
		}
		if len(waveErrs) > 0 {
			return in, errors.Join(waveErrs...)
		}

		// A node in this wave suspended the run. Completed siblings have
		// already been recorded above, so a later resume walk replays
		// them and re-derives the frontier. Stop here, leaving Input.Pause
		// set for the caller to surface and resume from.
		if paused {
			return in, nil
		}

		wave = nextCandidates
	}

	// Mark unvisited nodes as skipped so observers see a complete
	// per-node picture. A node left paused stays paused.
	for id := range c.Nodes {
		if !visited[id] && in.Status[id] != NodeStatusPaused {
			in.SetStatus(id, NodeStatusSkipped)
		}
	}
	return in, nil
}

func dedupUnvisited(ids []string, visited map[string]bool) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if visited[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
