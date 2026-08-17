package restate_runtime

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	restate "github.com/restatedev/sdk-go"
)

type RestateHistory struct {
	restateCtx         restate.WorkflowContext
	wrappedPersistence history.ConversationPersistenceAdapter
}

func NewRestateConversationPersistence(restateCtx restate.WorkflowContext, wrappedPersistence history.ConversationPersistenceAdapter) *RestateHistory {
	return &RestateHistory{
		restateCtx:         restateCtx,
		wrappedPersistence: wrappedPersistence,
	}
}

// NewConversationID generates a unique ID for a conversation
func (t *RestateHistory) NewConversationID(ctx context.Context) string {
	return restate.UUID(t.restateCtx).String()
}

func (t *RestateHistory) NewRunID(ctx context.Context) string {
	return restate.UUID(t.restateCtx).String()
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
