package history

import (
	"context"
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// texts flattens an outgoing provider list to its plain text, for asserting on
// ordering.
func texts(msgs []responses.InputMessageUnion) []string {
	var out []string
	for _, m := range msgs {
		if m.OfInputMessage == nil {
			out = append(out, "")
			continue
		}
		var b strings.Builder
		for _, c := range m.OfInputMessage.Content {
			if c.OfInputText != nil {
				b.WriteString(c.OfInputText.Text)
			}
		}
		out = append(out, b.String())
	}
	return out
}

func isSteeringNotice(s string) bool {
	return strings.Contains(s, "arrived from the user while this run was already in progress")
}

// TestSteeringNoticeFollowsQueuedMessage is the behaviour: a turn the user sent
// mid-run is followed by a note saying so, positioned right after it so the
// model reads them together.
func TestSteeringNoticeFollowsQueuedMessage(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	run.AddMessages(ctx, userTurn("original request"))
	run.AddMessagesToQueue(ctx, []Message{userTurn("actually, do it differently")})

	got := texts(mustGetMessages(t, run))

	if len(got) != 3 {
		t.Fatalf("outgoing list = %v, want 3 entries (turn, interjection, notice)", got)
	}
	if got[0] != "original request" {
		t.Errorf("out[0] = %q, want the opening turn", got[0])
	}
	if got[1] != "actually, do it differently" {
		t.Errorf("out[1] = %q, want the interjection verbatim", got[1])
	}
	if !isSteeringNotice(got[2]) {
		t.Errorf("out[2] = %q, want the steering notice", got[2])
	}
}

// TestSteeringNoticeOnlyForQueuedMessages guards the discrimination: a turn that
// opened the run is not an interjection and must not be labelled as one.
func TestSteeringNoticeOnlyForQueuedMessages(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	run.AddMessages(ctx, userTurn("just the opening turn"))
	run.AddMessages(ctx, assistantTurn("working on it"))

	for _, s := range texts(mustGetMessages(t, run)) {
		if isSteeringNotice(s) {
			t.Fatalf("steering notice attached to a message that was not queued: %q", s)
		}
	}
}

// TestSteeringNoticeIsNotPersisted pins the ephemerality. The note is about when
// a message arrived relative to the run receiving it; storing it would put
// harness prose into the thread's history and repeat it on every later turn.
func TestSteeringNoticeIsNotPersisted(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	run.AddMessagesToQueue(ctx, []Message{userTurn("interjection")})

	// The notice is in the outgoing list...
	var sawNotice bool
	for _, s := range texts(mustGetMessages(t, run)) {
		if isSteeringNotice(s) {
			sawNotice = true
		}
	}
	if !sawNotice {
		t.Fatal("expected a steering notice in the outgoing list")
	}

	run.RunState.TransitionToComplete()
	if err := run.SaveMessages(ctx); err != nil {
		t.Fatalf("SaveMessages: %v", err)
	}

	// ...but not in what was written to the thread.
	loaded, err := p.LoadMessages(ctx, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	for _, cmsg := range loaded {
		for _, b := range cmsg.Messages {
			for _, s := range texts(b.Messages) {
				if isSteeringNotice(s) {
					t.Fatalf("steering notice was persisted into history: %q", s)
				}
			}
		}
	}
}

// TestSteeringNoticeDoesNotSurviveTheRun covers the other half of ephemerality.
// On a later turn the interjection is simply an earlier message, so replaying
// the thread must not re-label it.
func TestSteeringNoticeDoesNotSurviveTheRun(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p)

	run1, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 1): %v", err)
	}
	run1.AddMessagesToQueue(ctx, []Message{userTurn("interjection")})
	mustGetMessages(t, run1)
	run1.RunState.TransitionToComplete()
	if err := run1.SaveMessages(ctx); err != nil {
		t.Fatalf("SaveMessages: %v", err)
	}

	run2, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun (turn 2): %v", err)
	}
	for _, s := range texts(mustGetMessages(t, run2)) {
		if isSteeringNotice(s) {
			t.Fatalf("steering notice reappeared on a later turn: %q", s)
		}
	}
}

// TestWithoutSteeringNotices covers the opt-out.
func TestWithoutSteeringNotices(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()
	cm := NewConversationManager(p, WithoutSteeringNotices())

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	run.AddMessagesToQueue(ctx, []Message{userTurn("interjection")})

	got := texts(mustGetMessages(t, run))
	if len(got) != 1 {
		t.Fatalf("outgoing list = %v, want just the interjection", got)
	}
	for _, s := range got {
		if isSteeringNotice(s) {
			t.Fatalf("notice emitted despite WithoutSteeringNotices: %q", s)
		}
	}
}

func mustGetMessages(t *testing.T, run *ConversationRunManager) []responses.InputMessageUnion {
	t.Helper()
	out, err := run.GetMessages(context.Background(), "agent")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	return out
}
