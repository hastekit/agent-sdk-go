package agents

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
)

// HistoryMiddleware wraps persistence reads and writes inside their execution
// boundary. Middleware may return without calling next. Errors propagate to
// the caller. Treat requests and returned messages as read-only; pass copies
// when transforming them. Registration order is outermost first.
type HistoryMiddleware interface {
	WrapLoadMessages(LoadMessagesFunc) LoadMessagesFunc
	WrapSaveMessages(SaveMessagesFunc) SaveMessagesFunc
}

type LoadMessagesRequest struct {
	Namespace     string `json:"namespace"`
	ThreadID      string `json:"thread_id"`
	PreviousRunID string `json:"previous_run_id,omitempty"`
}

type SaveMessagesRequest struct {
	Namespace      string            `json:"namespace"`
	RunID          string            `json:"run_id"`
	PreviousRunID  string            `json:"previous_run_id,omitempty"`
	ThreadID       string            `json:"thread_id"`
	ConversationID string            `json:"conversation_id"`
	Messages       []history.Message `json:"messages"`
	Meta           map[string]any    `json:"meta,omitempty"`
}

type LoadMessagesFunc func(context.Context, *LoadMessagesRequest) ([]history.ConversationMessage, error)
type SaveMessagesFunc func(context.Context, *SaveMessagesRequest) error

func ExecuteLoadMessagesWithMiddleware(ctx context.Context, middlewares []HistoryMiddleware, request *LoadMessagesRequest, next LoadMessagesFunc) ([]history.ConversationMessage, error) {
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] != nil {
			next = middlewares[i].WrapLoadMessages(next)
		}
	}
	return next(ctx, request)
}

func ExecuteSaveMessagesWithMiddleware(ctx context.Context, middlewares []HistoryMiddleware, request *SaveMessagesRequest, next SaveMessagesFunc) error {
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] != nil {
			next = middlewares[i].WrapSaveMessages(next)
		}
	}
	return next(ctx, request)
}

// WrapHistoryPersistence wraps only LoadMessages and SaveMessages. Runtime
// adapters wrap the real persistence here, then invoke it inside their existing
// activity/run step. IDs, clocks, summaries, listing and transcripts retain
// their existing behavior and optional capabilities.
func WrapHistoryPersistence(p history.ConversationPersistenceAdapter, middlewares ...HistoryMiddleware) history.ConversationPersistenceAdapter {
	if len(middlewares) == 0 {
		return p
	}
	wrapped := &historyMiddlewarePersistence{ConversationPersistenceAdapter: p, middlewares: middlewares}
	lister, lists := p.(history.ThreadLister)
	reader, reads := p.(history.TranscriptReader)
	switch {
	case lists && reads:
		return struct {
			*historyMiddlewarePersistence
			history.ThreadLister
			history.TranscriptReader
		}{wrapped, lister, reader}
	case lists:
		return struct {
			*historyMiddlewarePersistence
			history.ThreadLister
		}{wrapped, lister}
	case reads:
		return struct {
			*historyMiddlewarePersistence
			history.TranscriptReader
		}{wrapped, reader}
	default:
		return wrapped
	}
}

type historyMiddlewarePersistence struct {
	history.ConversationPersistenceAdapter
	middlewares []HistoryMiddleware
}

func (p *historyMiddlewarePersistence) LoadMessages(ctx context.Context, namespace, threadID, previousRunID string) ([]history.ConversationMessage, error) {
	request := &LoadMessagesRequest{Namespace: namespace, ThreadID: threadID, PreviousRunID: previousRunID}
	return ExecuteLoadMessagesWithMiddleware(ctx, p.middlewares, request, func(ctx context.Context, r *LoadMessagesRequest) ([]history.ConversationMessage, error) {
		return p.ConversationPersistenceAdapter.LoadMessages(ctx, r.Namespace, r.ThreadID, r.PreviousRunID)
	})
}

func (p *historyMiddlewarePersistence) SaveMessages(ctx context.Context, namespace, runID, previousRunID, threadID, conversationID string, messages []history.Message, meta map[string]any) error {
	request := &SaveMessagesRequest{Namespace: namespace, RunID: runID, PreviousRunID: previousRunID, ThreadID: threadID, ConversationID: conversationID, Messages: messages, Meta: meta}
	return ExecuteSaveMessagesWithMiddleware(ctx, p.middlewares, request, func(ctx context.Context, r *SaveMessagesRequest) error {
		return p.ConversationPersistenceAdapter.SaveMessages(ctx, r.Namespace, r.RunID, r.PreviousRunID, r.ThreadID, r.ConversationID, r.Messages, r.Meta)
	})
}
