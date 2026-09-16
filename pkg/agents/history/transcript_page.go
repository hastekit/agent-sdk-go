package history

import (
	"context"
	"errors"
)

// TranscriptPageOptions pages backwards through complete stored turns, not
// individual model messages. BeforeRunID is exclusive; empty selects the latest.
// Backends must order by insertion order, not by the lexical order of run IDs.
type TranscriptPageOptions struct {
	Limit       int
	BeforeRunID string
}

// TranscriptPage contains chronological rows and the latest thread state even
// when fetching an older page. NextBeforeRunID is empty at the start of history.
type TranscriptPage struct {
	Rows            []ConversationMessage
	NextBeforeRunID string
	Latest          *ConversationMessage
}

var ErrInvalidTranscriptCursor = errors.New("invalid or expired transcript cursor")

// TranscriptPageReader is optional. Database adapters should implement it with
// a bounded keyset query. Include Latest from the same read snapshot if possible.
type TranscriptPageReader interface {
	LoadTranscriptPage(context.Context, string, string, TranscriptPageOptions) (*TranscriptPage, error)
}

// LoadTranscriptPage uses native pagination when available. Legacy adapters
// remain compatible by loading their transcript and slicing it in memory.
func LoadTranscriptPage(ctx context.Context, adapter ConversationPersistenceAdapter, namespace, threadID string, opts TranscriptPageOptions) (*TranscriptPage, error) {
	if opts.Limit < 1 {
		return nil, errors.New("transcript page limit must be positive")
	}
	if reader, ok := adapter.(TranscriptPageReader); ok {
		return reader.LoadTranscriptPage(ctx, namespace, threadID, opts)
	}
	rows, err := LoadTranscript(ctx, adapter, namespace, threadID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].RunID
	}
	start, end, err := transcriptBounds(ids, opts)
	if err != nil {
		return nil, err
	}
	page := &TranscriptPage{Rows: rows[start:end]}
	if len(rows) > 0 {
		latest := rows[len(rows)-1]
		page.Latest = &latest
	}
	if start > 0 {
		page.NextBeforeRunID = rows[start].RunID
	}
	return page, nil
}

func transcriptBounds(ids []string, opts TranscriptPageOptions) (int, int, error) {
	if opts.Limit < 1 {
		return 0, 0, errors.New("transcript page limit must be positive")
	}
	end := len(ids)
	if opts.BeforeRunID != "" {
		end = -1
		for i, id := range ids {
			if id == opts.BeforeRunID {
				end = i
				break
			}
		}
		if end < 0 {
			return 0, 0, ErrInvalidTranscriptCursor
		}
	}
	return max(0, end-opts.Limit), end, nil
}

func (p *InMemoryConversationPersistence) LoadTranscriptPage(ctx context.Context, namespace, threadID string, opts TranscriptPageOptions) (*TranscriptPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	ids := p.messagesByThread[historyKey(namespace, threadID)]
	start, end, err := transcriptBounds(ids, opts)
	if err != nil {
		return nil, err
	}
	row := func(id string) ConversationMessage {
		m := p.messages[historyKey(namespace, id)]
		return ConversationMessage{RunID: m.RunID, ThreadID: m.ThreadID, ConversationID: m.ConversationID, Messages: m.Messages, Meta: m.Meta}
	}
	page := &TranscriptPage{Rows: make([]ConversationMessage, 0, end-start)}
	for _, id := range ids[start:end] {
		page.Rows = append(page.Rows, row(id))
	}
	if len(ids) > 0 {
		latest := row(ids[len(ids)-1])
		page.Latest = &latest
	}
	if start > 0 {
		page.NextBeforeRunID = ids[start]
	}
	return page, nil
}

func (p *FileConversationPersistence) LoadTranscriptPage(ctx context.Context, namespace, threadID string, opts TranscriptPageOptions) (*TranscriptPage, error) {
	return p.mem.LoadTranscriptPage(ctx, namespace, threadID, opts)
}
