package restate_runtime

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/stretchr/testify/require"
)

type persistenceMiddleware struct {
	agents.NoopMiddleware
	operations *[]string
}

func (m persistenceMiddleware) WrapLoadMessages(next agents.LoadMessagesFunc) agents.LoadMessagesFunc {
	return func(ctx context.Context, r *agents.LoadMessagesRequest) ([]history.ConversationMessage, error) {
		*m.operations = append(*m.operations, "load")
		rows, err := next(ctx, r)
		return append(rows, history.ConversationMessage{ThreadID: "from middleware"}), err
	}
}

func (m persistenceMiddleware) WrapSaveMessages(next agents.SaveMessagesFunc) agents.SaveMessagesFunc {
	return func(ctx context.Context, r *agents.SaveMessagesRequest) error {
		*m.operations = append(*m.operations, "save")
		return next(ctx, r)
	}
}

func (m persistenceMiddleware) WrapGetPrompt(agents.GetPromptFunc) agents.GetPromptFunc {
	return func(context.Context, *agents.Dependencies) (string, error) {
		*m.operations = append(*m.operations, "prompt")
		return "from middleware", nil
	}
}

// These are the bound operations invoked by the restate.Run callbacks. Binding
// them must not execute middleware in workflow code or wrap it in its own step.
func TestRestateHistoryAndPromptBindMiddlewareToRunBodies(t *testing.T) {
	var operations []string
	m := persistenceMiddleware{operations: &operations}
	store := history.NewInMemoryConversationPersistence()
	h := NewRestateConversationPersistence(nil, store, m)
	p := NewRestatePrompt(nil, nil, m).(*RestatePrompt)
	require.Empty(t, operations)
	require.NoError(t, h.wrappedPersistence.SaveMessages(t.Context(), "tenant", "run", "", "thread", "conversation", nil, nil))
	rows, err := h.wrappedPersistence.LoadMessages(t.Context(), "tenant", "thread", "")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "from middleware", rows[1].ThreadID)
	prompt, err := p.wrappedPrompt.GetPrompt(t.Context(), &agents.Dependencies{})
	require.NoError(t, err)
	require.Equal(t, "from middleware", prompt)
	require.Equal(t, []string{"save", "load", "prompt"}, operations)
}
