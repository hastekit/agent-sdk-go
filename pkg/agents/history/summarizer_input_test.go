package history

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// capturingSummarizer records what it was handed and replays a caller-supplied
// decision back.
type capturingSummarizer struct {
	sawMessages []Message
	sawRunIDs   map[string]string
	result      *SummaryResult
}

func (c *capturingSummarizer) Summarize(ctx context.Context, msgIdToRunId map[string]string, msgs []Message, contextTokens int) (*SummaryResult, error) {
	c.sawMessages = append([]Message(nil), msgs...)
	c.sawRunIDs = map[string]string{}
	for k, v := range msgIdToRunId {
		c.sawRunIDs[k] = v
	}
	return c.result, nil
}

func assistantTurn(text string) Message {
	return messages.New("agent", []responses.InputMessageUnion{{
		OfInputMessage: &responses.InputMessage{
			Role:    constants.RoleAssistant,
			Content: responses.InputContent{{OfInputText: &responses.InputTextContent{Text: text}}},
		},
	}})
}

// TestSummarizerSeesInFlightMessages is the core of the fix: a run's own output
// is the fastest-growing part of the request, and it used to be invisible to
// the summarizer because it lives in the save buffer until the run ends.
func TestSummarizerSeesInFlightMessages(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cap := &capturingSummarizer{}
	cm := NewConversationManager(p, WithSummarizer(cap))

	// Turn 1 leaves one saved message behind.
	run1, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 1): %v", err)
	}
	run1.AddMessages(ctx, userTurn("hello"))
	run1.RunState.TransitionToComplete()
	if err := run1.SaveMessages(ctx); err != nil {
		t.Fatalf("SaveMessages: %v", err)
	}

	// Turn 2: a tool loop appends three more messages before any save.
	run2, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 2): %v", err)
	}
	run2.AddMessages(ctx, userTurn("second question"))
	run2.AddMessages(ctx, assistantTurn("calling a tool"))
	run2.AddMessages(ctx, assistantTurn("calling another"))

	if _, err := run2.GetMessages(ctx, "agent"); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	// 1 loaded + 3 in flight.
	if got := len(cap.sawMessages); got != 4 {
		t.Fatalf("summarizer saw %d messages, want 4 (1 loaded + 3 in flight)", got)
	}
}

// TestInFlightMessagesCarryRunID checks the grouping input, not just the count.
// Without a run id, every in-flight bundle looks up to "" and the summarizer
// pools them into a single nameless run.
func TestInFlightMessagesCarryRunID(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cap := &capturingSummarizer{}
	cm := NewConversationManager(p, WithSummarizer(cap))

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	first := userTurn("hello")
	second := assistantTurn("working on it")
	run.AddMessages(ctx, first)
	run.AddMessages(ctx, second)

	if _, err := run.GetMessages(ctx, "agent"); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	runID := run.GetRunID()
	for _, m := range []Message{first, second} {
		if got := cap.sawRunIDs[m.ID]; got != runID {
			t.Errorf("bundle %s mapped to run %q, want %q", m.ID, got, runID)
		}
	}
}

// TestQueuedMessagesReachTheModel pins what queued turns are actually for:
// they drain into the outgoing list, and they carry this run's id from the
// moment they were queued. The summarizer deliberately does not see them on the
// call that drains them — it could not act on them anyway, since they belong to
// the run in flight, which every summarizer keeps whole.
func TestQueuedMessagesReachTheModel(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cap := &capturingSummarizer{}
	cm := NewConversationManager(p, WithSummarizer(cap))

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	queued := userTurn("queued while the run was working")
	run.AddMessagesToQueue(ctx, []Message{queued})

	out, err := run.GetMessages(ctx, "agent")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	if len(out) == 0 {
		t.Fatal("outgoing list is empty; the queued turn should have been drained into it")
	}
	if len(run.RunState.QueuedMessages) != 0 {
		t.Errorf("QueuedMessages = %d, want 0 (drained)", len(run.RunState.QueuedMessages))
	}
	if got := run.msgIdToRunId[queued.ID]; got != run.GetRunID() {
		t.Errorf("queued bundle mapped to run %q, want %q", got, run.GetRunID())
	}
}

// TestSummarizationPreservesSaveBuffer pins the safety property. newMessages is
// the save buffer — SaveMessages persists exactly those — so a summarizer that
// drops in-flight messages must not shrink it, or the thread's history gets a
// hole where the turn should be.
func TestSummarizationPreservesSaveBuffer(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()

	// A summarizer that keeps nothing at all.
	cap := &capturingSummarizer{result: &SummaryResult{
		SummaryID:      "sum-1",
		MessagesToKeep: nil,
	}}
	cm := NewConversationManager(p, WithSummarizer(cap))

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	inFlight := userTurn("this turn must still be saved")
	run.AddMessages(ctx, inFlight)

	out, err := run.GetMessages(ctx, "agent")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("outgoing list is empty; the in-flight turn should still be sent")
	}

	run.RunState.TransitionToComplete()
	if err := run.SaveMessages(ctx); err != nil {
		t.Fatalf("SaveMessages: %v", err)
	}

	loaded, err := p.LoadMessages(ctx, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	var found bool
	for _, cmsg := range loaded {
		for _, b := range cmsg.Messages {
			if b.ID == inFlight.ID {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("in-flight message was dropped from persistence by summarization")
	}
}

// TestSummaryBoundaryNeverNamesInFlightRun guards the persistence boundary.
// Reloading drops every row through LastSummarizedMessageID, so naming the run
// currently being written would discard messages this turn is still producing.
func TestSummaryBoundaryNeverNamesInFlightRun(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cap := &capturingSummarizer{}
	cm := NewConversationManager(p, WithSummarizer(cap))

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	run.AddMessages(ctx, userTurn("hello"))

	// The summarizer claims to have covered the run in flight.
	cap.result = &SummaryResult{
		SummaryID:           "sum-1",
		LastSummarizedRunID: run.GetRunID(),
	}

	if _, err := run.GetMessages(ctx, "agent"); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	if got := run.summaries.LastSummarizedRunID; got != "" {
		t.Fatalf("LastSummarizedMessageID = %q, want \"\" (clamped off the in-flight run)", got)
	}
}

// TestGetMessagesDoesNotAliasHistory covers the append aliasing:
// `append(cm.oldMessages, cm.newMessages...)` writes into oldMessages' spare
// capacity whenever it has any, so a filter mutating the returned slice would
// corrupt the stored history.
func TestGetMessagesDoesNotAliasHistory(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	// Give oldMessages spare capacity, which is what makes append alias.
	old := make([]Message, 1, 8)
	old[0] = userTurn("history")
	run.oldMessages = old
	kept := old[0].ID

	run.AddMessages(ctx, assistantTurn("in flight"))

	if _, err := run.GetMessages(ctx, "agent"); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	if got := run.oldMessages[0].ID; got != kept {
		t.Fatalf("oldMessages[0] = %q, want %q — history was overwritten", got, kept)
	}
	if got := len(run.oldMessages); got != 1 {
		t.Fatalf("len(oldMessages) = %d, want 1", got)
	}
}
