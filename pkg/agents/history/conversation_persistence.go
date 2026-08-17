package history

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"go.opentelemetry.io/otel/attribute"
)

// inMemoryMessage represents a message with its ordering metadata
type inMemoryMessage struct {
	RunID          string
	PreviousRunID  string
	ThreadID       string
	ConversationID string
	Namespace      string
	Messages       []Message
	Meta           map[string]any
	CreatedAt      time.Time
}

// inMemoryThread represents a thread with its message chain
type inMemoryThread struct {
	ThreadID       string
	ConversationID string
	OriginRunID    string
	LastRunID      string
	Namespace      string
	CreatedAt      time.Time
}

// inMemorySummary represents a conversation summary
type inMemorySummary struct {
	ID                  string
	ThreadID            string
	Namespace           string
	SummaryMessage      messages.Message
	LastSummarizedRunID string
	CreatedAt           time.Time
	Meta                map[string]any
}

// InMemoryConversationPersistence is a simple in-memory implementation of ConversationPersistenceAdapter
type InMemoryConversationPersistence struct {
	mu sync.RWMutex

	// messages indexed by messageID
	messages map[string]*inMemoryMessage
	// threads indexed by threadID
	threads map[string]*inMemoryThread
	// summaries indexed by threadID (latest summary per thread)
	summaries map[string]*inMemorySummary
	// messagesByThread: threadID -> ordered list of messageIDs
	messagesByThread map[string][]string
}

// NewInMemoryConversationPersistence creates a new in-memory conversation persistence adapter
func NewInMemoryConversationPersistence() *InMemoryConversationPersistence {
	return &InMemoryConversationPersistence{
		messages:         make(map[string]*inMemoryMessage),
		threads:          make(map[string]*inMemoryThread),
		summaries:        make(map[string]*inMemorySummary),
		messagesByThread: make(map[string][]string),
	}
}

// NewConversationID generates a unique ID for a conversation
func (p *InMemoryConversationPersistence) NewConversationID(ctx context.Context) string {
	return uuid.NewString()
}

// NewRunID generates a unique ID for a run
func (p *InMemoryConversationPersistence) NewRunID(ctx context.Context) string {
	return uuid.NewString()
}

// LoadMessages retrieves all messages up to and including the previousRunID
func (p *InMemoryConversationPersistence) LoadMessages(ctx context.Context, namespace string, threadID string, previousRunId string) ([]ConversationMessage, error) {
	ctx, span := tracer.Start(ctx, "InMemoryConversationPersistence.LoadMessages")
	defer span.End()

	span.SetAttributes(
		attribute.String("namespace", namespace),
		attribute.String("thread_id", threadID),
		attribute.String("previous_run_id", previousRunId),
	)

	if threadID == "" {
		return []ConversationMessage{}, nil
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	// Get the thread
	thread, exists := p.threads[threadID]
	if !exists {
		return []ConversationMessage{}, nil
	}

	// Check if there's a summary for this thread
	summary, hasSummary := p.summaries[threadID]

	// Get the ordered messages for this thread
	messageIDs, exists := p.messagesByThread[threadID]
	if !exists {
		return []ConversationMessage{}, nil
	}

	var result []ConversationMessage

	// If we have a summary, check if it's applicable
	if hasSummary && summary.LastSummarizedRunID != "" {
		// Find the position of the last summarized message
		summarizedIdx := -1
		targetIdx := -1
		for i, runID := range messageIDs {
			if runID == summary.LastSummarizedRunID {
				summarizedIdx = i
			}
			if runID == previousRunId {
				targetIdx = i
			}
		}

		// If the summary covers messages before the target, use it
		if summarizedIdx >= 0 && summarizedIdx < targetIdx {
			// Add the summary as the first message
			summaryMsg := ConversationMessage{
				RunID:          summary.ID,
				ThreadID:       summary.ThreadID,
				ConversationID: thread.ConversationID,
				Messages:       []Message{summary.SummaryMessage},
				Meta:           summary.Meta,
			}
			result = append(result, summaryMsg)

			// Add messages after the summarized point up to and including the target
			for i := summarizedIdx + 1; i <= targetIdx; i++ {
				m := p.messages[messageIDs[i]]
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
	}

	// No applicable summary, return all messages up to and including previousRunID
	for _, runID := range messageIDs {
		m := p.messages[runID]
		result = append(result, ConversationMessage{
			RunID:          m.RunID,
			ThreadID:       m.ThreadID,
			ConversationID: m.ConversationID,
			Messages:       m.Messages,
			Meta:           m.Meta,
		})

		if runID == previousRunId {
			break
		}
	}

	span.SetAttributes(attribute.Int("conversation_messages_count", len(result)))

	return result, nil
}

// SaveMessages saves messages with support for conversations and threads
func (p *InMemoryConversationPersistence) SaveMessages(ctx context.Context, namespace, runId, previousRunId, threadId, conversationId string, messages []Message, meta map[string]any) error {
	ctx, span := tracer.Start(ctx, "InMemoryConversationPersistence.SaveMessages")
	defer span.End()

	span.SetAttributes(
		attribute.String("namespace", namespace),
		attribute.String("previous_run_id", previousRunId),
		attribute.String("thread_id", threadId),
		attribute.String("conversation_id", conversationId),
		attribute.Int("messages_count", len(messages)),
	)

	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// Incremental save of an already-saved run. A single run can be
	// saved more than once under the same run id — e.g. it pauses for a
	// tool/approval round and is later continued — and each save carries
	// only the messages added since the previous one. Append those to
	// the existing record instead of overwriting it (which would drop
	// the earlier messages, often the user turn that opened the run) and
	// don't re-index the run in its thread.
	if existing, ok := p.messages[runId]; ok {
		existing.Messages = append(existing.Messages, messages...)
		if meta != nil {
			existing.Meta = meta
		}
		if t := p.threads[existing.ThreadID]; t != nil {
			t.LastRunID = runId
		}
		return nil
	}

	var threadID string
	var convID string

	// Case 1: Starting a new conversation with a provided conversationId
	if previousRunId == "" {
		convID = conversationId
		if convID == "" {
			convID = uuid.NewString()
		}

		threadID = threadId
		if threadID == "" {
			threadID = uuid.NewString()
		}

		// Create a new thread
		p.threads[threadID] = &inMemoryThread{
			ThreadID:       threadID,
			OriginRunID:    runId,
			ConversationID: convID,
			LastRunID:      runId,
			Namespace:      namespace,
			CreatedAt:      now,
		}
		p.messagesByThread[threadID] = []string{}
	} else if previousRunId != "" {
		// Case 2: Continuing an existing conversation
		prevMsg, exists := p.messages[previousRunId]
		if !exists {
			// Previous message doesn't exist, create a new thread
			convID = conversationId
			if convID == "" {
				convID = uuid.New().String()
			}
			threadID = uuid.New().String()

			p.threads[threadID] = &inMemoryThread{
				ThreadID:       threadID,
				ConversationID: convID,
				OriginRunID:    runId,
				LastRunID:      runId,
				Namespace:      namespace,
				CreatedAt:      now,
			}
			p.messagesByThread[threadID] = []string{}
		} else {
			// Continue in the existing thread
			threadID = prevMsg.ThreadID
			convID = prevMsg.ConversationID

			thread := p.threads[threadID]
			if thread != nil {
				// Check if we're branching (previousRunId is not the last message)
				if thread.LastRunID != previousRunId {
					// Create a new thread (branch)
					newThreadID := uuid.New().String()
					threadID = newThreadID

					// Copy messages up to previousRunId to the new thread
					oldMessages := p.messagesByThread[prevMsg.ThreadID]
					newMessages := []string{}
					for _, oldMsgID := range oldMessages {
						newMessages = append(newMessages, oldMsgID)
						if oldMsgID == previousRunId {
							break
						}
					}

					p.threads[newThreadID] = &inMemoryThread{
						ThreadID:       newThreadID,
						ConversationID: convID,
						OriginRunID:    prevMsg.ThreadID, // Reference to original thread
						LastRunID:      runId,
						Namespace:      namespace,
						CreatedAt:      now,
					}
					p.messagesByThread[newThreadID] = newMessages
				} else {
					// Update the last message ID
					thread.LastRunID = runId
				}
			}
		}
	} else {
		// Case 3: Starting a completely new conversation
		convID = uuid.New().String()
		threadID = uuid.New().String()

		p.threads[threadID] = &inMemoryThread{
			ThreadID:       threadID,
			ConversationID: convID,
			OriginRunID:    runId,
			LastRunID:      runId,
			Namespace:      namespace,
			CreatedAt:      now,
		}
		p.messagesByThread[threadID] = []string{}
	}

	// Create and store the message
	p.messages[runId] = &inMemoryMessage{
		RunID:          runId,
		PreviousRunID:  previousRunId,
		ThreadID:       threadID,
		ConversationID: convID,
		Namespace:      namespace,
		Messages:       messages,
		Meta:           meta,
		CreatedAt:      now,
	}

	// Add message to the thread's message list
	p.messagesByThread[threadID] = append(p.messagesByThread[threadID], runId)

	return nil
}

// SaveSummary saves a conversation summary for a thread
func (p *InMemoryConversationPersistence) SaveSummary(ctx context.Context, namespace string, summary Summary) error {
	ctx, span := tracer.Start(ctx, "InMemoryConversationPersistence.SaveSummary")
	defer span.End()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.summaries[summary.ThreadID] = &inMemorySummary{
		ID:                  summary.ID,
		ThreadID:            summary.ThreadID,
		Namespace:           namespace,
		SummaryMessage:      summary.SummaryMessage,
		LastSummarizedRunID: summary.LastSummarizedRunID,
		CreatedAt:           summary.CreatedAt,
		Meta:                summary.Meta,
	}

	return nil
}

// getMessage returns the stored message for an ID, or nil if absent
func (p *InMemoryConversationPersistence) getMessage(runID string) *inMemoryMessage {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.messages[runID]
}

// getThread returns the stored thread for an ID, or nil if absent
func (p *InMemoryConversationPersistence) getThread(threadID string) *inMemoryThread {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.threads[threadID]
}

// Clear removes all stored data (useful for testing)
func (p *InMemoryConversationPersistence) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.messages = make(map[string]*inMemoryMessage)
	p.threads = make(map[string]*inMemoryThread)
	p.summaries = make(map[string]*inMemorySummary)
	p.messagesByThread = make(map[string][]string)
}

// GetMessageCount returns the total number of messages (useful for testing)
func (p *InMemoryConversationPersistence) GetMessageCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.messages)
}

// GetThreadCount returns the total number of threads (useful for testing)
func (p *InMemoryConversationPersistence) GetThreadCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.threads)
}
