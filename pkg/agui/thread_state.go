package agui

import (
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
)

// ThreadRunState is what a thread's last run left behind, for a client that
// has just loaded the page and has no run to learn it from.
//
// A pause is the case that needs it. The approval card is drawn from the
// on_interrupt event a run emits, and a browser that refreshes has no run and
// therefore no event — so the pending decision simply vanishes from the screen
// while the run sits waiting for it. Everything here is read from the same
// transcript the messages come from, so the two cannot disagree.
type ThreadRunState struct {
	RunID  string `json:"runId,omitempty"`
	Status string `json:"status,omitempty"`

	// AwaitingApproval reports that a decision is outstanding, not merely that
	// the run is paused: a pause on an elicitation wants data or a visited
	// URL, not a verdict.
	AwaitingApproval bool `json:"awaitingApproval"`

	// Interrupts are the pending pauses, in the shape the on_interrupt event
	// carries them — so a client can draw the same card from either.
	Interrupts []map[string]any `json:"interrupts,omitempty"`

	// PendingToolCalls is the older approval-only projection, kept for clients
	// written against it.
	PendingToolCalls []map[string]any `json:"pendingToolCalls,omitempty"`

	// BackgroundTasks are the tasks the thread is still waiting on. They carry
	// from run to run and are dropped as their results arrive, so a task that
	// outlives several turns is still reported and a finished one is not.
	BackgroundTasks []ThreadBackgroundTask `json:"backgroundTasks,omitempty"`
}

// ThreadBackgroundTask is one task the thread has in flight.
type ThreadBackgroundTask struct {
	TaskID   string `json:"taskId"`
	CallID   string `json:"callId,omitempty"`
	ToolName string `json:"toolName,omitempty"`

	// StreamID is the task's own channel, so a client can watch its progress
	// without having seen the run that started it.
	StreamID  string `json:"streamId,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
}

// threadRunState reads the state of the last run in a transcript.
//
// Nil when there is nothing to say — no rows, no run meta, or a run that
// finished cleanly with nothing outstanding — so a client can treat its
// presence as "there is something here to act on".
func threadRunState(rows []history.ConversationMessage) *ThreadRunState {
	if len(rows) == 0 {
		return nil
	}

	last := rows[len(rows)-1]
	runState := agentstate.LoadRunStateFromMeta(last.Meta)
	if runState == nil {
		return nil
	}

	out := &ThreadRunState{RunID: last.RunID}
	if pending := runState.PendingInterrupts(); len(pending) > 0 {
		out.Status = string(agentstate.RunStatusPaused)
		out.Interrupts = projectInterrupts(pending)
		out.PendingToolCalls = projectPendingToolCalls(approvalCalls(pending))
		out.AwaitingApproval = len(out.PendingToolCalls) > 0
	}

	for _, task := range runState.BackgroundTaskList() {
		entry := ThreadBackgroundTask{
			TaskID:   task.TaskID,
			CallID:   task.CallID,
			ToolName: task.ToolName,
			StreamID: task.StreamID,
		}
		if !task.StartedAt.IsZero() {
			entry.StartedAt = task.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		out.BackgroundTasks = append(out.BackgroundTasks, entry)
	}

	if out.Status == "" && len(out.BackgroundTasks) == 0 {
		return nil
	}
	return out
}
