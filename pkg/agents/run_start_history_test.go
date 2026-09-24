package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type historyAtStartBroker struct {
	*streambroker.MemoryStreamBroker
	onStart func(agents.RunEvent)
}

func (b *historyAtStartBroker) PublishRunEvent(ctx context.Context, event agents.RunEvent) error {
	if event.Event == agents.RunEventStarted {
		b.onStart(event)
	}
	return b.MemoryStreamBroker.PublishRunEvent(ctx, event)
}

func TestOpeningMessageIsStoredBeforeRunStart(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			var store history.ConversationPersistenceAdapter = history.NewInMemoryConversationPersistence()
			if backend == "file" {
				var err error
				store, err = history.NewFileConversationPersistence(t.TempDir())
				require.NoError(t, err)
			}
			ctx := t.Context()
			llm := &scriptedLLM{script: []*responses.Response{textResponse("first answer"), textResponse("second answer")}}
			starts := 0
			broker := &historyAtStartBroker{MemoryStreamBroker: streambroker.NewMemoryStreamBroker()}
			broker.onStart = func(event agents.RunEvent) {
				require.Equal(t, starts, llm.callCount(), "persist before calling the model")
				starts++
				threads, err := store.(history.ThreadLister).ListThreads(ctx, "tenant", "default")
				require.NoError(t, err)
				require.Len(t, threads, 1)
				require.Equal(t, "Plan my trip", threads[0].Title)
				require.Equal(t, event.RunID, threads[0].LastRunID)
				rows, err := store.LoadMessages(ctx, "tenant", "thread", "")
				require.NoError(t, err)
				require.Len(t, rows, starts)
				latest := rows[len(rows)-1]
				require.Len(t, latest.Messages, 1)
				require.Equal(t, event.RunID, latest.RunID)
				require.NotContains(t, latest.Meta, agentstate.CompletedAtMetaKey)
				require.EqualValues(t, agentstate.RunStatusInProgress, latest.Meta["run_state"].(map[string]any)["status"])
			}
			a := newScriptedAgent("test", llm, history.NewConversationManager(store), broker, nil, nil)
			for i, prompt := range []string{"Plan my trip", "Add a museum"} {
				out, err := a.ExecuteLocal(ctx, &agents.AgentInput{Namespace: "tenant", ThreadID: "thread", Message: userMessage(prompt)})
				require.NoError(t, err)
				require.Equal(t, agentstate.RunStatusCompleted, out.Status)
				rows, err := store.LoadMessages(ctx, "tenant", "thread", "")
				require.NoError(t, err)
				require.Len(t, rows, i+1)
				require.Len(t, rows[i].Messages, 2, "completion appends without duplicating the opening message")
				require.Contains(t, rows[i].Meta, agentstate.CompletedAtMetaKey)
			}
			require.Equal(t, 2, starts)
		})
	}
}

type failedOpeningSave struct {
	*history.InMemoryConversationPersistence
	err error
}

func (p *failedOpeningSave) SaveMessages(context.Context, string, string, string, string, string, string, []history.Message, map[string]any) error {
	return p.err
}

func TestOpeningSaveFailureStopsBeforeModelAndStartEvent(t *testing.T) {
	failure := errors.New("history unavailable")
	store := &failedOpeningSave{history.NewInMemoryConversationPersistence(), failure}
	llm := &scriptedLLM{}
	broker := &historyAtStartBroker{MemoryStreamBroker: streambroker.NewMemoryStreamBroker(), onStart: func(agents.RunEvent) {
		t.Fatal("start event published before successful persistence")
	}}
	a := newScriptedAgent("test", llm, history.NewConversationManager(store), broker, nil, nil)
	out, err := a.ExecuteLocal(t.Context(), &agents.AgentInput{ThreadID: "thread", Message: userMessage("hello")})
	require.ErrorIs(t, err, failure)
	require.Equal(t, agentstate.RunStatusError, out.Status)
	require.Zero(t, llm.callCount())
}

// Failure diagnostics are UI metadata, never synthetic messages in the next model request.
func TestFailedRunErrorIsExcludedFromFollowUpModelInput(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			var store history.ConversationPersistenceAdapter = history.NewInMemoryConversationPersistence()
			if backend == "file" {
				var err error
				store, err = history.NewFileConversationPersistence(t.TempDir())
				require.NoError(t, err)
			}

			// Exhaust the model once so the real failure path persists a diagnostic.
			model := &scriptedLLM{}
			agent := newScriptedAgent("test", model, history.NewConversationManager(store), streambroker.NewMemoryStreamBroker(), nil, nil)
			failed, failure := agent.ExecuteLocal(t.Context(), &agents.AgentInput{Namespace: "default", ThreadID: "thread", Message: userMessage("original question")})
			require.Error(t, failure)
			rows, err := store.LoadMessages(t.Context(), "default", "thread", "")
			require.NoError(t, err)
			require.Equal(t, failure.Error(), agentstate.LoadRunStateFromMeta(rows[0].Meta).Error)

			// Recreate the agent to exercise reloading persisted history rather than in-memory state.
			nextModel := &scriptedLLM{script: []*responses.Response{textResponse("recovered")}}
			nextAgent := newScriptedAgent("test", nextModel, history.NewConversationManager(store), streambroker.NewMemoryStreamBroker(), nil, nil)
			next, err := nextAgent.ExecuteLocal(t.Context(), &agents.AgentInput{Namespace: "default", ThreadID: "thread", Message: userMessage("please try again")})
			require.NoError(t, err)
			require.NotEqual(t, failed.RunID, next.RunID)

			// Inspect the complete provider request, not only its displayed transcript.
			request, err := json.Marshal(nextModel.request(0))
			require.NoError(t, err)
			require.Contains(t, string(request), "original question")
			require.Contains(t, string(request), "please try again")
			require.NotContains(t, string(request), failure.Error())
			require.NotContains(t, string(request), "scripted LLM exhausted")
		})
	}
}
