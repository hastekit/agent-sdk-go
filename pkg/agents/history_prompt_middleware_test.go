package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type operationMiddleware struct {
	agents.NoopMiddleware
	name         string
	log          *[]string
	shortCircuit bool
	failure      error
}

func (m operationMiddleware) enter(operation string) func() {
	*m.log = append(*m.log, m.name+" before "+operation)
	return func() { *m.log = append(*m.log, m.name+" after "+operation) }
}

func (m operationMiddleware) WrapLoadMessages(next agents.LoadMessagesFunc) agents.LoadMessagesFunc {
	return func(ctx context.Context, r *agents.LoadMessagesRequest) ([]history.ConversationMessage, error) {
		defer m.enter("load")()
		if m.shortCircuit {
			return []history.ConversationMessage{{ThreadID: "cached"}}, m.failure
		}
		return next(ctx, r)
	}
}

func (m operationMiddleware) WrapSaveMessages(next agents.SaveMessagesFunc) agents.SaveMessagesFunc {
	return func(ctx context.Context, r *agents.SaveMessagesRequest) error {
		defer m.enter("save")()
		if m.shortCircuit {
			return m.failure
		}
		copy := *r
		copy.Meta = map[string]any{"source": m.name}
		return next(ctx, &copy)
	}
}

func (m operationMiddleware) WrapGetPrompt(next agents.GetPromptFunc) agents.GetPromptFunc {
	return func(ctx context.Context, deps *agents.Dependencies) (string, error) {
		defer m.enter("prompt")()
		if m.shortCircuit {
			return "cached prompt", m.failure
		}
		prompt, err := next(ctx, deps)
		return prompt + " " + m.name, err
	}
}

func TestHistoryAndPromptMiddlewareOrderAndShortCircuit(t *testing.T) {
	for _, shortCircuit := range []bool{false, true} {
		for _, operation := range []string{"load", "save", "prompt"} {
			t.Run(operation+map[bool]string{false: " chain", true: " cached"}[shortCircuit], func(t *testing.T) {
				var log []string
				outer := operationMiddleware{name: "outer", log: &log, shortCircuit: shortCircuit}
				inner := operationMiddleware{name: "inner", log: &log}
				middlewares := []agents.Middleware{outer, nil, inner}
				switch operation {
				case "load":
					result, err := agents.ExecuteLoadMessagesWithMiddleware(t.Context(), agents.HistoryMiddlewaresOf(middlewares), &agents.LoadMessagesRequest{Namespace: "tenant"}, func(_ context.Context, r *agents.LoadMessagesRequest) ([]history.ConversationMessage, error) {
						log = append(log, "call load")
						require.Equal(t, "tenant", r.Namespace)
						return []history.ConversationMessage{{ThreadID: "stored"}}, nil
					})
					require.NoError(t, err)
					require.Len(t, result, 1)
					require.Equal(t, map[bool]string{false: "stored", true: "cached"}[shortCircuit], result[0].ThreadID)
				case "save":
					r := &agents.SaveMessagesRequest{Namespace: "tenant", Meta: map[string]any{"source": "original"}}
					err := agents.ExecuteSaveMessagesWithMiddleware(t.Context(), agents.HistoryMiddlewaresOf(middlewares), r, func(_ context.Context, r *agents.SaveMessagesRequest) error {
						log = append(log, "call save")
						require.Equal(t, "inner", r.Meta["source"])
						return nil
					})
					require.NoError(t, err)
					require.Equal(t, "original", r.Meta["source"])
				case "prompt":
					result, err := agents.ExecuteGetPromptWithMiddleware(t.Context(), agents.PromptMiddlewaresOf(middlewares), &agents.Dependencies{}, func(context.Context, *agents.Dependencies) (string, error) {
						log = append(log, "call prompt")
						return "base", nil
					})
					require.NoError(t, err)
					require.Equal(t, map[bool]string{false: "base inner outer", true: "cached prompt"}[shortCircuit], result)
				}
				if shortCircuit {
					require.Equal(t, []string{"outer before " + operation, "outer after " + operation}, log)
				} else {
					require.Equal(t, []string{"outer before " + operation, "inner before " + operation, "call " + operation, "inner after " + operation, "outer after " + operation}, log)
				}
			})
		}
	}
}

func TestHistoryAndPromptMiddlewareErrors(t *testing.T) {
	failure := errors.New("unavailable")
	var log []string
	m := operationMiddleware{log: &log, shortCircuit: true, failure: failure}
	// Nil providers are deliberate: a short circuit must not invoke them.
	p := agents.WrapHistoryPersistence(nil, m)
	_, err := p.LoadMessages(t.Context(), "tenant", "thread", "")
	require.ErrorIs(t, err, failure)
	err = p.SaveMessages(t.Context(), "tenant", "run", "", "thread", "conversation", nil, nil)
	require.ErrorIs(t, err, failure)
	_, err = agents.WrapPromptProvider(nil, m).GetPrompt(t.Context(), nil)
	require.ErrorIs(t, err, failure)
}

func TestAgentBindsHistoryAndPromptMiddlewareWithoutMutatingSharedManager(t *testing.T) {
	store := history.NewInMemoryConversationPersistence()
	manager := history.NewConversationManager(store)
	var log []string
	a := agents.NewAgent(&agents.AgentOptions{Name: "agent", History: manager, Middlewares: []agents.Middleware{operationMiddleware{name: "audit", log: &log}}}).WithLLM(&scriptedLLM{script: []*responses.Response{textResponse("done")}})
	runAgent(t, a, &agents.AgentInput{Namespace: "tenant", ThreadID: "thread", Message: userMessage("hi")})
	require.Contains(t, log, "audit before load")
	require.Contains(t, log, "audit before save")
	require.Contains(t, log, "audit before prompt")
	require.Same(t, store, manager.ConversationPersistenceAdapter)
	require.NotSame(t, manager, a.History())
	_, lists := a.History().ConversationPersistenceAdapter.(history.ThreadLister)
	_, reads := a.History().ConversationPersistenceAdapter.(history.TranscriptReader)
	require.True(t, lists)
	require.True(t, reads)
	stored, err := store.LoadMessages(t.Context(), "tenant", "thread", "")
	require.NoError(t, err)
	require.NotEmpty(t, stored)
	require.Equal(t, "audit", stored[len(stored)-1].Meta["source"])
}

func TestHistoryMiddlewarePreservesOptionalCapabilities(t *testing.T) {
	store := history.NewInMemoryConversationPersistence()
	plain := struct {
		history.ConversationPersistenceAdapter
	}{store}
	listing := struct {
		history.ConversationPersistenceAdapter
		history.ThreadLister
	}{store, store}
	transcript := struct {
		history.ConversationPersistenceAdapter
		history.TranscriptReader
	}{store, store}
	for _, p := range []history.ConversationPersistenceAdapter{plain, listing, transcript, store} {
		wrapped := agents.WrapHistoryPersistence(p, agents.NoopMiddleware{})
		_, lists := p.(history.ThreadLister)
		_, wrappedLists := wrapped.(history.ThreadLister)
		require.Equal(t, lists, wrappedLists)
		_, reads := p.(history.TranscriptReader)
		_, wrappedReads := wrapped.(history.TranscriptReader)
		require.Equal(t, reads, wrappedReads)
	}
}
