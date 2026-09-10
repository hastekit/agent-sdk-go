package restate_runtime

import (
	"context"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	restate "github.com/restatedev/sdk-go"
)

type RestateHistory struct {
	restateCtx         restate.WorkflowContext
	wrappedPersistence history.ConversationPersistenceAdapter
}

func NewRestateConversationPersistence(restateCtx restate.WorkflowContext, wrappedPersistence history.ConversationPersistenceAdapter, middlewares ...agents.HistoryMiddleware) *RestateHistory {
	return &RestateHistory{
		restateCtx:         restateCtx,
		wrappedPersistence: agents.WrapHistoryPersistence(wrappedPersistence, middlewares...),
	}
}

// NewConversationID generates a unique ID for a conversation
func (t *RestateHistory) NewConversationID(ctx context.Context) string {
	return restate.UUID(t.restateCtx).String()
}

func (t *RestateHistory) NewRunID(ctx context.Context) string {
	return restate.UUID(t.restateCtx).String()
}

// Now reads the clock inside a journaled step, so a replay is handed the
// recorded reading rather than taking a fresh one. Unix nanoseconds cross the
// journal rather than a time.Time, which carries a monotonic reading and a
// location that do not survive the round trip.
func (t *RestateHistory) Now(ctx context.Context) time.Time {
	nanos, err := restate.Run(t.restateCtx, func(restate.RunContext) (int64, error) {
		return time.Now().UnixNano(), nil
	}, restate.WithName("Now"))
	if err != nil {
		return time.Now().UTC()
	}
	return time.Unix(0, nanos).UTC()
}

func (t *RestateHistory) LoadMessages(ctx context.Context, namespace string, threadID string, previousRunID string) ([]history.ConversationMessage, error) {
	return restate.Run(t.restateCtx, func(ctx restate.RunContext) ([]history.ConversationMessage, error) {
		return t.wrappedPersistence.LoadMessages(ctx, namespace, threadID, previousRunID)
	}, restate.WithName("LoadMessages"))
}

func (t *RestateHistory) SaveMessages(ctx context.Context, namespace, runId, previousRunId, threadId, conversationId string, messages []history.Message, meta map[string]any) error {
	_, err := restate.Run(t.restateCtx, func(ctx restate.RunContext) (any, error) {
		return nil, t.wrappedPersistence.SaveMessages(ctx, namespace, runId, previousRunId, threadId, conversationId, messages, meta)
	}, restate.WithName("SaveMessages"))
	return err
}

func (t *RestateHistory) SaveSummary(ctx context.Context, namespace string, summary history.Summary) error {
	_, err := restate.Run(t.restateCtx, func(ctx restate.RunContext) (any, error) {
		return nil, t.wrappedPersistence.SaveSummary(ctx, namespace, summary)
	}, restate.WithName("SaveSummary"))
	return err
}
