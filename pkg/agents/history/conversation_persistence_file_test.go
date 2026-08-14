package history

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

func newTestMessage(t *testing.T, sender, text string) Message {
	t.Helper()
	return messages.New(sender, []responses.InputMessageUnion{responses.UserMessage(text)})
}

// conversationDigest reduces loaded messages to comparable essentials,
// avoiding DeepEqual noise from JSON round-tripping of message unions.
type conversationDigest struct {
	MessageID      string
	ThreadID       string
	ConversationID string
	BundleIDs      []string
	Senders        []string
}

func digest(msgs []ConversationMessage) []conversationDigest {
	var out []conversationDigest
	for _, m := range msgs {
		d := conversationDigest{
			MessageID:      m.RunID,
			ThreadID:       m.ThreadID,
			ConversationID: m.ConversationID,
		}
		for _, bundle := range m.Messages {
			d.BundleIDs = append(d.BundleIDs, bundle.ID)
			d.Senders = append(d.Senders, bundle.SenderID)
		}
		out = append(out, d)
	}
	return out
}

func TestFileConversationPersistenceRoundtrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	p, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("failed to create persistence: %v", err)
	}

	meta := map[string]any{"step": "complete"}
	if err := p.SaveMessages(ctx, "ns", "msg-1", "", "thread-1", "conv-1", []Message{newTestMessage(t, "alice", "hello")}, meta); err != nil {
		t.Fatalf("failed to save msg-1: %v", err)
	}
	if err := p.SaveMessages(ctx, "ns", "msg-2", "msg-1", "thread-1", "conv-1", []Message{newTestMessage(t, "bob", "hi back")}, meta); err != nil {
		t.Fatalf("failed to save msg-2: %v", err)
	}

	loaded, err := p.LoadMessages(ctx, "ns", "thread-1", "msg-2")
	if err != nil {
		t.Fatalf("failed to load messages: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(loaded))
	}
	if err := p.Close(); err != nil {
		t.Fatalf("failed to close: %v", err)
	}

	// Reopen from the same directory and verify the replayed state serves
	// the same conversation.
	reopened, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("failed to reopen persistence: %v", err)
	}
	defer reopened.Close()

	replayed, err := reopened.LoadMessages(ctx, "ns", "thread-1", "msg-2")
	if err != nil {
		t.Fatalf("failed to load replayed messages: %v", err)
	}

	if !reflect.DeepEqual(digest(loaded), digest(replayed)) {
		t.Fatalf("replayed messages differ:\noriginal: %+v\nreplayed: %+v", digest(loaded), digest(replayed))
	}

	if got := replayed[1].Meta["step"]; got != "complete" {
		t.Fatalf("expected meta to round-trip, got %v", got)
	}

	// Round-tripping may decode into a different union variant
	// (OfEasyInput vs OfInputMessage), so compare the serialized form.
	raw, err := sonic.Marshal(replayed[0].Messages[0].Messages[0])
	if err != nil {
		t.Fatalf("failed to marshal replayed message: %v", err)
	}
	if !strings.Contains(string(raw), "hello") {
		t.Fatalf("expected message text to round-trip, got %s", raw)
	}
}

func TestFileConversationPersistenceBranchReplay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	p, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("failed to create persistence: %v", err)
	}

	if err := p.SaveMessages(ctx, "ns", "msg-1", "", "thread-1", "conv-1", []Message{newTestMessage(t, "alice", "first")}, nil); err != nil {
		t.Fatalf("failed to save msg-1: %v", err)
	}
	if err := p.SaveMessages(ctx, "ns", "msg-2", "msg-1", "thread-1", "conv-1", []Message{newTestMessage(t, "alice", "second")}, nil); err != nil {
		t.Fatalf("failed to save msg-2: %v", err)
	}
	// Branch off msg-1: SaveMessages mints a new thread containing msg-1 + msg-3
	if err := p.SaveMessages(ctx, "ns", "msg-3", "msg-1", "thread-1", "conv-1", []Message{newTestMessage(t, "alice", "branched")}, nil); err != nil {
		t.Fatalf("failed to save msg-3: %v", err)
	}

	branchThreadID := p.mem.getMessage("msg-3").ThreadID
	if branchThreadID == "thread-1" {
		t.Fatalf("expected msg-3 to land on a new branch thread")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("failed to close: %v", err)
	}

	reopened, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("failed to reopen persistence: %v", err)
	}
	defer reopened.Close()

	if !reflect.DeepEqual(p.mem.messagesByThread, reopened.mem.messagesByThread) {
		t.Fatalf("replayed thread indexes differ:\noriginal: %+v\nreplayed: %+v", p.mem.messagesByThread, reopened.mem.messagesByThread)
	}

	for threadID, thread := range p.mem.threads {
		replayedThread, ok := reopened.mem.threads[threadID]
		if !ok {
			t.Fatalf("replayed state missing thread %s", threadID)
		}
		if replayedThread.LastRunID != thread.LastRunID {
			t.Fatalf("thread %s last message: expected %s, got %s", threadID, thread.LastRunID, replayedThread.LastRunID)
		}
		if replayedThread.ConversationID != thread.ConversationID {
			t.Fatalf("thread %s conversation: expected %s, got %s", threadID, thread.ConversationID, replayedThread.ConversationID)
		}
	}

	loaded, err := reopened.LoadMessages(ctx, "ns", branchThreadID, "msg-3")
	if err != nil {
		t.Fatalf("failed to load branch thread: %v", err)
	}
	if len(loaded) != 2 || loaded[0].RunID != "msg-1" || loaded[1].RunID != "msg-3" {
		t.Fatalf("expected branch thread [msg-1 msg-3], got %+v", digest(loaded))
	}
}

func TestFileConversationPersistenceMultipleConversations(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	p, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("failed to create persistence: %v", err)
	}

	if err := p.SaveMessages(ctx, "ns", "msg-a1", "", "thread-a", "conv-a", []Message{newTestMessage(t, "alice", "a1")}, nil); err != nil {
		t.Fatalf("failed to save msg-a1: %v", err)
	}
	if err := p.SaveMessages(ctx, "ns", "msg-a2", "msg-a1", "thread-a", "conv-a", []Message{newTestMessage(t, "alice", "a2")}, nil); err != nil {
		t.Fatalf("failed to save msg-a2: %v", err)
	}
	if err := p.SaveMessages(ctx, "ns", "msg-b1", "", "thread-b", "conv-b", []Message{newTestMessage(t, "bob", "b1")}, nil); err != nil {
		t.Fatalf("failed to save msg-b1: %v", err)
	}
	// Empty conversation ID resolves to a generated one at save time
	if err := p.SaveMessages(ctx, "ns", "msg-c1", "", "", "", []Message{newTestMessage(t, "carol", "c1")}, nil); err != nil {
		t.Fatalf("failed to save msg-c1: %v", err)
	}

	storedC := p.mem.getMessage("msg-c1")
	if storedC.ConversationID == "" {
		t.Fatalf("expected msg-c1 to get a generated conversation ID")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("failed to close: %v", err)
	}

	// Each conversation gets its own JSONL file
	for _, convID := range []string{"conv-a", "conv-b", storedC.ConversationID} {
		if _, err := os.Stat(filepath.Join(dir, conversationFileName(convID))); err != nil {
			t.Fatalf("expected conversation file for %s: %v", convID, err)
		}
	}

	reopened, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("failed to reopen persistence: %v", err)
	}
	defer reopened.Close()

	for threadID, want := range map[string][]string{
		"thread-a":       {"msg-a1", "msg-a2"},
		"thread-b":       {"msg-b1"},
		storedC.ThreadID: {"msg-c1"},
	} {
		loaded, err := reopened.LoadMessages(ctx, "ns", threadID, want[len(want)-1])
		if err != nil {
			t.Fatalf("failed to load thread %s: %v", threadID, err)
		}
		var got []string
		for _, m := range loaded {
			got = append(got, m.RunID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("thread %s: expected %v, got %v", threadID, want, got)
		}
	}
}

func TestFileConversationPersistenceSummaryReplay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	p, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("failed to create persistence: %v", err)
	}

	for _, ids := range [][2]string{{"msg-1", ""}, {"msg-2", "msg-1"}, {"msg-3", "msg-2"}} {
		if err := p.SaveMessages(ctx, "ns", ids[0], ids[1], "thread-1", "conv-1", []Message{newTestMessage(t, "alice", ids[0])}, nil); err != nil {
			t.Fatalf("failed to save %s: %v", ids[0], err)
		}
	}

	summary := Summary{
		ID:                  "summary-1",
		ThreadID:            "thread-1",
		SummaryMessage:      newTestMessage(t, "system", "summary of msg-1"),
		LastSummarizedRunID: "msg-1",
		CreatedAt:           time.Now(),
		Meta:                map[string]any{"is_summary": true},
	}
	if err := p.SaveSummary(ctx, "ns", summary); err != nil {
		t.Fatalf("failed to save summary: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("failed to close: %v", err)
	}

	reopened, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("failed to reopen persistence: %v", err)
	}
	defer reopened.Close()

	loaded, err := reopened.LoadMessages(ctx, "ns", "thread-1", "msg-3")
	if err != nil {
		t.Fatalf("failed to load messages: %v", err)
	}

	if len(loaded) != 3 {
		t.Fatalf("expected summary + 2 messages, got %d: %+v", len(loaded), digest(loaded))
	}
	if loaded[0].RunID != "summary-1" {
		t.Fatalf("expected first message to be the summary, got %s", loaded[0].RunID)
	}
	if loaded[1].RunID != "msg-2" || loaded[2].RunID != "msg-3" {
		t.Fatalf("expected [msg-2 msg-3] after summary, got %+v", digest(loaded))
	}
}

func TestFileConversationPersistenceSkipsPartialTrailingLine(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	p, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("failed to create persistence: %v", err)
	}
	if err := p.SaveMessages(ctx, "ns", "msg-1", "", "thread-1", "conv-1", []Message{newTestMessage(t, "alice", "hello")}, nil); err != nil {
		t.Fatalf("failed to save msg-1: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("failed to close: %v", err)
	}

	// Simulate a write torn by a crash: a partial record with no newline.
	f, err := os.OpenFile(filepath.Join(dir, conversationFileName("conv-1")), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("failed to open conversation file: %v", err)
	}
	if _, err := f.WriteString(`{"type":"message","message":{"run_id":"msg-2","thread`); err != nil {
		t.Fatalf("failed to write partial line: %v", err)
	}
	f.Close()

	reopened, err := NewFileConversationPersistence(dir)
	if err != nil {
		t.Fatalf("expected partial trailing line to be skipped, got: %v", err)
	}
	defer reopened.Close()

	loaded, err := reopened.LoadMessages(ctx, "ns", "thread-1", "msg-1")
	if err != nil {
		t.Fatalf("failed to load messages: %v", err)
	}
	if len(loaded) != 1 || loaded[0].RunID != "msg-1" {
		t.Fatalf("expected only msg-1 to survive, got %+v", digest(loaded))
	}
}
