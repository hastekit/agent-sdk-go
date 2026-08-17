package temporal_runtime

import (
	"context"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"go.temporal.io/sdk/workflow"
)

type TemporalHistory struct {
	wrappedPersistence history.ConversationPersistenceAdapter
}

func NewTemporalConversationPersistence(wrappedPersistence history.ConversationPersistenceAdapter) *TemporalHistory {
	return &TemporalHistory{
		wrappedPersistence: wrappedPersistence,
	}
}

func (t *TemporalHistory) LoadMessages(ctx context.Context, namespace string, threadID string, previousRunID string) ([]history.ConversationMessage, error) {
	return t.wrappedPersistence.LoadMessages(ctx, namespace, threadID, previousRunID)
}

func (t *TemporalHistory) SaveMessages(ctx context.Context, namespace, runId, previousRunId, threadId, conversationId string, messages []history.Message, meta map[string]any) error {
	return t.wrappedPersistence.SaveMessages(ctx, namespace, runId, previousRunId, threadId, conversationId, messages, meta)
}

func (t *TemporalHistory) SaveSummary(ctx context.Context, namespace string, summary history.Summary) error {
	return t.wrappedPersistence.SaveSummary(ctx, namespace, summary)
}

type TemporalConversationPersistenceProxy struct {
	workflowCtx workflow.Context
	prefix      string
}

func NewTemporalConversationPersistenceProxy(workflowCtx workflow.Context, prefix string) history.ConversationPersistenceAdapter {
	return &TemporalConversationPersistenceProxy{
		workflowCtx: workflowCtx,
		prefix:      prefix,
	}
}

// NewConversationID generates a unique ID for a conversation
func (t *TemporalConversationPersistenceProxy) NewConversationID(ctx context.Context) string {
	idAny := workflow.SideEffect(t.workflowCtx, func(ctx workflow.Context) interface{} {
		return uuid.NewString()
	})

	var id string
	if err := idAny.Get(&id); err != nil {
		return uuid.NewString() // ideally, we won't get here as uuid.NewString() is not supposed to throw errors
	}

	return id
}

func (t *TemporalConversationPersistenceProxy) NewRunID(ctx context.Context) string {
	idAny := workflow.SideEffect(t.workflowCtx, func(ctx workflow.Context) interface{} {
		return uuid.NewString()
	})

	var id string
	if err := idAny.Get(&id); err != nil {
		return uuid.NewString() // ideally, we won't get here as uuid.NewString() is not supposed to throw errors
	}

	return id
}

func (t *TemporalConversationPersistenceProxy) LoadMessages(ctx context.Context, namespace string, threadID string, previousRunID string) ([]history.ConversationMessage, error) {
	var messages []history.ConversationMessage
	err := workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_LoadMessagesActivity", namespace, threadID, previousRunID).Get(t.workflowCtx, &messages)
	if err != nil {
		return messages, err
	}

	return messages, nil
}

func (t *TemporalConversationPersistenceProxy) SaveMessages(ctx context.Context, namespace, runId, previousRunId, threadId, conversationId string, messages []history.Message, meta map[string]any) error {
	return workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_SaveMessagesActivity", namespace, runId, previousRunId, threadId, conversationId, messages, meta).Get(t.workflowCtx, nil)
}

func (t *TemporalConversationPersistenceProxy) SaveSummary(ctx context.Context, namespace string, summary history.Summary) error {
	return workflow.ExecuteActivity(t.workflowCtx, t.prefix+"_SaveSummaryActivity", namespace, summary).Get(t.workflowCtx, nil)
}
