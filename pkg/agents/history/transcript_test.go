package history

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// summarizedThread stores three turns and a summary covering the first, which
// is the state a long conversation reaches on its own.
func summarizedThread(t *testing.T) *InMemoryConversationPersistence {
	t.Helper()
	ctx := context.Background()

	p := NewInMemoryConversationPersistence()
	for _, ids := range [][2]string{{"msg-1", ""}, {"msg-2", "msg-1"}, {"msg-3", "msg-2"}} {
		require.NoError(t, p.SaveMessages(ctx, "ns", ids[0], ids[1], "thread-1", "conv-1",
			[]Message{newTestMessage(t, "alice", ids[0])}, nil))
	}
	require.NoError(t, p.SaveSummary(ctx, "ns", Summary{
		ID:                      "summary-1",
		ThreadID:                "thread-1",
		SummaryMessage:          newTestMessage(t, "system", "summary of msg-1"),
		LastSummarizedMessageID: "msg-1",
		CreatedAt:               time.Now(),
		Meta:                    map[string]any{"is_summary": true},
	}))
	return p
}

func messageIDs(rows []ConversationMessage) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.MessageID)
	}
	return ids
}

// The transcript is what the user actually said, summary or no summary.
func TestLoadTranscript_IgnoresTheSummary(t *testing.T) {
	p := summarizedThread(t)

	rows, err := p.LoadTranscript(t.Context(), "ns", "thread-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"msg-1", "msg-2", "msg-3"}, messageIDs(rows))
}

// The contrast that motivates the method: the model's view of the same thread
// has the summarized turn replaced. A UI hydrating from this loses msg-1.
func TestLoadMessages_SubstitutesTheSummary(t *testing.T) {
	p := summarizedThread(t)

	rows, err := p.LoadMessages(t.Context(), "ns", "thread-1", "msg-3")
	require.NoError(t, err)
	assert.Equal(t, []string{"summary-1", "msg-2", "msg-3"}, messageIDs(rows))
}

// An adapter that can't read transcripts still answers, with the summarized
// view — the same thing it returned before the method existed.
func TestLoadTranscript_FallsBackForPlainAdapters(t *testing.T) {
	p := summarizedThread(t)

	rows, err := LoadTranscript(t.Context(), summaryOnlyAdapter{inner: p}, "ns", "thread-1")
	require.NoError(t, err)
	assert.NotEmpty(t, rows)
}

// summaryOnlyAdapter implements the persistence adapter and nothing more,
// standing in for one written before TranscriptReader existed. It delegates
// explicitly rather than embedding, so it cannot inherit LoadTranscript by
// accident and quietly stop testing the fallback.
type summaryOnlyAdapter struct {
	inner *InMemoryConversationPersistence
}

func (a summaryOnlyAdapter) NewConversationID(ctx context.Context) string {
	return a.inner.NewConversationID(ctx)
}

func (a summaryOnlyAdapter) NewRunID(ctx context.Context) string {
	return a.inner.NewRunID(ctx)
}

func (a summaryOnlyAdapter) LoadMessages(ctx context.Context, namespace, threadID, previousMessageID string) ([]ConversationMessage, error) {
	return a.inner.LoadMessages(ctx, namespace, threadID, previousMessageID)
}

func (a summaryOnlyAdapter) SaveMessages(ctx context.Context, namespace, msgID, previousMsgID, threadID, conversationID string, msgs []Message, meta map[string]any) error {
	return a.inner.SaveMessages(ctx, namespace, msgID, previousMsgID, threadID, conversationID, msgs, meta)
}

func (a summaryOnlyAdapter) SaveSummary(ctx context.Context, namespace string, summary Summary) error {
	return a.inner.SaveSummary(ctx, namespace, summary)
}

func TestLoadTranscript_UnknownThread(t *testing.T) {
	p := NewInMemoryConversationPersistence()

	rows, err := p.LoadTranscript(t.Context(), "ns", "nope")
	require.NoError(t, err)
	assert.Empty(t, rows)
}
