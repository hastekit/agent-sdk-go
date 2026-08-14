package history

import "context"

// TranscriptReader is an optional capability of persistence adapters that can
// return a thread exactly as it was written — every turn, in order, with no
// summary standing in for any of them.
//
// It exists because LoadMessages answers a different question. LoadMessages
// builds the *model's* view of a thread: once the thread has been summarized
// it returns the summary in place of the turns the summary covers, which is
// what keeps a long conversation inside the context window. A chat UI
// hydrating from that view shows the user their own conversation with the
// early part replaced by a machine-written paragraph — and the turns are still
// on disk, so nothing signals that anything is missing.
type TranscriptReader interface {
	LoadTranscript(ctx context.Context, namespace, threadID string) ([]ConversationMessage, error)
}

// LoadTranscript returns a thread as written, for display.
//
// Adapters that cannot do it fall back to LoadMessages — no worse than not
// asking, but a summarized thread will read as summarized. If your UI shows
// history, implementing TranscriptReader on your adapter is what makes the
// early turns come back.
func LoadTranscript(ctx context.Context, adapter ConversationPersistenceAdapter, namespace, threadID string) ([]ConversationMessage, error) {
	if reader, ok := adapter.(TranscriptReader); ok {
		return reader.LoadTranscript(ctx, namespace, threadID)
	}
	return adapter.LoadMessages(ctx, namespace, threadID, "")
}

// LoadTranscript returns the thread as written, for display. See the package
// function of the same name for what "as written" is protecting against.
func (cm *CommonConversationManager) LoadTranscript(ctx context.Context, namespace, threadID string) ([]ConversationMessage, error) {
	if cm.ConversationPersistenceAdapter == nil {
		return nil, nil
	}
	return LoadTranscript(ctx, cm.ConversationPersistenceAdapter, namespace, threadID)
}

// LoadTranscript returns every turn stored for the thread, in order. Summaries
// live in their own store and are not turns, so they simply never appear here.
func (p *InMemoryConversationPersistence) LoadTranscript(ctx context.Context, namespace, threadID string) ([]ConversationMessage, error) {
	ctx, span := tracer.Start(ctx, "InMemoryConversationPersistence.LoadTranscript")
	defer span.End()

	if threadID == "" {
		return []ConversationMessage{}, nil
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	messageIDs, ok := p.messagesByThread[threadID]
	if !ok {
		return []ConversationMessage{}, nil
	}

	result := make([]ConversationMessage, 0, len(messageIDs))
	for _, msgID := range messageIDs {
		m := p.messages[msgID]
		if m == nil {
			continue
		}
		result = append(result, ConversationMessage{
			RunID:          m.RunID,
			ThreadID:       m.ThreadID,
			ConversationID: m.ConversationID,
			Messages:       m.Messages,
			Meta:           m.Meta,
		})
	}

	return result, nil
}

// LoadTranscript returns every turn stored for the thread, in order.
func (p *FileConversationPersistence) LoadTranscript(ctx context.Context, namespace, threadID string) ([]ConversationMessage, error) {
	return p.mem.LoadTranscript(ctx, namespace, threadID)
}
