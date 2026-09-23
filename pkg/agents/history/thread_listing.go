package history

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Routine context keys are persisted with each scheduler-triggered turn.
const (
	RoutineIDContextKey    = "routine_id"
	RoutineAgentContextKey = "routine_agent"
)

// ThreadInfo summarizes a stored thread for listing UIs.
type ThreadInfo struct {
	GroupID        string    `json:"group_id,omitempty"`
	AgentName      string    `json:"agent_name,omitempty"`
	ThreadID       string    `json:"thread_id"`
	ConversationID string    `json:"conversation_id"`
	Namespace      string    `json:"namespace"`
	LastRunID      string    `json:"last_run_id"`
	Title          string    `json:"title"`
	MessageCount   int       `json:"message_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ThreadLister is an optional capability of persistence adapters that
// can enumerate stored threads. Pass namespace "" to list across all
// namespaces. An empty group ID selects DefaultGroupID (normal conversations);
// a nonempty group ID matches that group exactly.
type ThreadLister interface {
	ListThreads(ctx context.Context, namespace, groupID string) ([]ThreadInfo, error)
}

// ListThreads returns the stored threads, newest first.
func (p *InMemoryConversationPersistence) ListThreads(ctx context.Context, namespace, groupID string) ([]ThreadInfo, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	threads := make([]ThreadInfo, 0, len(p.threads))
	for _, t := range p.threads {
		if (namespace != "" && t.Namespace != namespace) || t.GroupID != NormalizeGroupID(groupID) {
			continue
		}

		info := ThreadInfo{
			GroupID:        t.GroupID,
			ThreadID:       t.ThreadID,
			ConversationID: t.ConversationID,
			Namespace:      t.Namespace,
			LastRunID:      t.LastRunID,
			MessageCount:   len(p.messagesByThread[historyKey(t.Namespace, t.ThreadID)]),
			CreatedAt:      t.CreatedAt,
			UpdatedAt:      t.CreatedAt,
		}

		// Title comes from the thread's first textual user message —
		// what a chat UI shows as the conversation name. UpdatedAt is
		// the newest turn's save time so listings sort by recency.
		for _, runID := range p.messagesByThread[historyKey(t.Namespace, t.ThreadID)] {
			m := p.messages[historyKey(t.Namespace, runID)]
			if m == nil {
				continue
			}
			updatedAt := m.UpdatedAt
			if updatedAt.IsZero() {
				updatedAt = m.CreatedAt
			}
			if updatedAt.After(info.UpdatedAt) {
				info.UpdatedAt = updatedAt
			}
			if info.AgentName == "" {
				if rc, ok := m.Meta[RunContextMetaKey].(map[string]any); ok {
					info.AgentName, _ = rc[RoutineAgentContextKey].(string)
				}
			}
			if info.Title == "" {
				info.Title = titleFromBundles(m.Messages)
			}
		}

		threads = append(threads, info)
	}

	sort.SliceStable(threads, func(i, j int) bool {
		return threads[i].UpdatedAt.After(threads[j].UpdatedAt)
	})

	return threads, nil
}

// titleFromBundles extracts the first user-authored text from a
// turn's sender-attributed bundles, truncated to a label-friendly
// length.
func titleFromBundles(bundles []Message) string {
	for _, bundle := range bundles {
		for _, msg := range bundle.Messages {
			text := ""
			switch {
			case msg.OfEasyInput != nil:
				if s := msg.OfEasyInput.Content.OfString; s != nil {
					text = *s
				} else {
					for _, c := range msg.OfEasyInput.Content.OfInputMessageList {
						if c.OfInputText != nil && c.OfInputText.Text != "" {
							text = c.OfInputText.Text
							break
						}
					}
				}
			case msg.OfInputMessage != nil:
				for _, c := range msg.OfInputMessage.Content {
					if c.OfInputText != nil && c.OfInputText.Text != "" {
						text = c.OfInputText.Text
						break
					}
				}
			}
			if text = strings.TrimSpace(text); text != "" {
				return truncateTitle(text)
			}
		}
	}
	return ""
}

func truncateTitle(s string) string {
	const max = 80
	if line, _, found := strings.Cut(s, "\n"); found {
		s = line
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// ListThreads returns the stored threads, newest first.
func (p *FileConversationPersistence) ListThreads(ctx context.Context, namespace, groupID string) ([]ThreadInfo, error) {
	return p.mem.ListThreads(ctx, namespace, groupID)
}
