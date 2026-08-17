package history

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func userBundle(sender, text string) Message {
	return messages.New(sender, []responses.InputMessageUnion{responses.UserMessage(text)})
}

func TestListThreadsInMemory(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()

	require.NoError(t, p.SaveMessages(ctx, "ns", "m1", "", "thread-a", "conv-a",
		[]Message{userBundle("user", "first question about Go")}, nil))
	require.NoError(t, p.SaveMessages(ctx, "ns", "m2", "m1", "thread-a", "conv-a",
		[]Message{userBundle("user", "follow-up")}, nil))
	require.NoError(t, p.SaveMessages(ctx, "ns", "m3", "", "thread-b", "conv-b",
		[]Message{userBundle("user", "another topic")}, nil))
	require.NoError(t, p.SaveMessages(ctx, "other-ns", "m4", "", "thread-c", "conv-c",
		[]Message{userBundle("user", "hidden")}, nil))

	threads, err := p.ListThreads(ctx, "ns")
	require.NoError(t, err)
	require.Len(t, threads, 2)

	byID := map[string]ThreadInfo{}
	for _, th := range threads {
		byID[th.ThreadID] = th
	}
	a := byID["thread-a"]
	assert.Equal(t, "first question about Go", a.Title)
	assert.Equal(t, 2, a.MessageCount)
	assert.Equal(t, "conv-a", a.ConversationID)
	assert.False(t, a.UpdatedAt.Before(a.CreatedAt))

	b := byID["thread-b"]
	assert.Equal(t, "another topic", b.Title)

	// Namespace filter excluded thread-c.
	_, hidden := byID["thread-c"]
	assert.False(t, hidden)

	// Empty namespace lists everything.
	all, err := p.ListThreads(ctx, "")
	require.NoError(t, err)
	assert.Len(t, all, 3)
}

func TestListThreadsFilePersistenceSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	p, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	require.NoError(t, p.SaveMessages(ctx, "ns", "m1", "", "thread-a", "conv-a",
		[]Message{userBundle("user", "persisted question")}, nil))
	require.NoError(t, p.Close())

	reopened, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	threads, err := reopened.ListThreads(ctx, "ns")
	require.NoError(t, err)
	require.Len(t, threads, 1)
	assert.Equal(t, "thread-a", threads[0].ThreadID)
	assert.Equal(t, "persisted question", threads[0].Title)
}

func TestTitleTruncation(t *testing.T) {
	long := ""
	for i := 0; i < 30; i++ {
		long += "hello "
	}
	title := truncateTitle(long)
	assert.LessOrEqual(t, len([]rune(title)), 81)
	assert.Contains(t, title, "…")

	assert.Equal(t, "first line", truncateTitle("first line\nsecond line"))
}

func TestListThreadsOrderedByUpdatedAtDesc(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()

	require.NoError(t, p.SaveMessages(ctx, "ns", "m1", "", "thread-a", "conv-a",
		[]Message{userBundle("user", "older")}, nil))
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, p.SaveMessages(ctx, "ns", "m2", "", "thread-b", "conv-b",
		[]Message{userBundle("user", "newer")}, nil))

	threads, err := p.ListThreads(ctx, "ns")
	require.NoError(t, err)
	require.Len(t, threads, 2)
	assert.Equal(t, "thread-b", threads[0].ThreadID)
	assert.Equal(t, "thread-a", threads[1].ThreadID)
}

// flattenInner returns every inner provider-message's first text across
// all conversation rows, for asserting nothing was dropped.
func flattenInner(rows []ConversationMessage) []string {
	var out []string
	for _, r := range rows {
		for _, b := range r.Messages {
			for _, inner := range b.Messages {
				if inner.OfInputMessage != nil {
					for _, c := range inner.OfInputMessage.Content {
						if c.OfInputText != nil {
							out = append(out, c.OfInputText.Text)
						}
					}
				}
				// File persistence round-trips an InputMessage's JSON
				// back into the EasyInput arm of the union.
				if inner.OfEasyInput != nil {
					if s := inner.OfEasyInput.Content.OfString; s != nil {
						out = append(out, *s)
					}
					for _, c := range inner.OfEasyInput.Content.OfInputMessageList {
						if c.OfInputText != nil {
							out = append(out, c.OfInputText.Text)
						}
					}
				}
				if inner.OfOutputMessage != nil && inner.OfOutputMessage.Content != nil {
					for _, c := range *inner.OfOutputMessage.Content {
						if c.OfOutputText != nil {
							out = append(out, c.OfOutputText.Text)
						}
					}
				}
			}
		}
	}
	return out
}

func TestSaveMessagesMergesSameRunID(t *testing.T) {
	ctx := context.Background()
	p := NewInMemoryConversationPersistence()

	// Turn 1: run A completes.
	require.NoError(t, p.SaveMessages(ctx, "ns", "runA", "", "T", "C",
		[]Message{userBundle("user", "how are you")}, map[string]any{}))
	// Turn 2: run B, first incremental save (opens with the user turn).
	require.NoError(t, p.SaveMessages(ctx, "ns", "runB", "runA", "T", "C",
		[]Message{userBundle("user", "tell me a joke")}, map[string]any{}))
	// Turn 2: run B continues under the SAME id (tool/approval round).
	require.NoError(t, p.SaveMessages(ctx, "ns", "runB", "runB", "T", "C",
		[]Message{userBundle("agent", "here is a joke")}, map[string]any{}))

	rows, err := p.LoadMessages(ctx, "ns", "T", "")
	require.NoError(t, err)

	// Two run records (no duplicate runB row), and the user message that
	// opened run B survives the second save.
	require.Len(t, rows, 2)
	texts := flattenInner(rows)
	assert.Contains(t, texts, "how are you")
	assert.Contains(t, texts, "tell me a joke")
	assert.Contains(t, texts, "here is a joke")
}

func TestFileReplayMergesSameRunID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	p, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	require.NoError(t, p.SaveMessages(ctx, "ns", "runA", "", "T", "C",
		[]Message{userBundle("user", "how are you")}, map[string]any{}))
	require.NoError(t, p.SaveMessages(ctx, "ns", "runB", "runA", "T", "C",
		[]Message{userBundle("user", "tell me a joke")}, map[string]any{}))
	require.NoError(t, p.SaveMessages(ctx, "ns", "runB", "runB", "T", "C",
		[]Message{userBundle("agent", "here is a joke")}, map[string]any{}))
	require.NoError(t, p.Close())

	// Reopen → replay from disk must reconstruct the merged run, not
	// drop the earlier increment.
	reopened, err := NewFileConversationPersistence(dir)
	require.NoError(t, err)
	rows, err := reopened.LoadMessages(ctx, "ns", "T", "")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	texts := flattenInner(rows)
	assert.Contains(t, texts, "how are you")
	assert.Contains(t, texts, "tell me a joke")
	assert.Contains(t, texts, "here is a joke")
}
