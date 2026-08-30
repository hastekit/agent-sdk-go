package agents_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func storedRuns(t *testing.T, agent *agents.Agent, ns, threadID, runID string) []history.ConversationMessage {
	t.Helper()
	loaded, err := agent.History().ConversationPersistenceAdapter.LoadMessages(
		context.Background(), ns, threadID, runID)
	require.NoError(t, err)
	require.NotEmpty(t, loaded)
	return loaded
}

// Every bundle a run records is identified, and by a uuid — the loop runs
// inside a workflow under the durable runtimes, so the id comes from the
// runtime's journaled source rather than a draw that differs on each replay.
func TestEveryBundleIsIdentifiedByAUUID(t *testing.T) {
	tool := newFakeTool("lookup", false, "found it")
	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call_1", "lookup", `{}`),
		textResponse("here you go"),
	}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:  "atlas",
		Tools: []agents.Tool{tool},
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-ids",
		Message:   userMessage("look it up"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	seen := map[string]bool{}
	count := 0
	for _, run := range storedRuns(t, agent, "test", "thread-ids", out.RunID) {
		for _, b := range run.Messages {
			count++
			parsed, err := uuid.Parse(b.ID)
			require.NoError(t, err, "message id %q is not a uuid", b.ID)
			assert.Equal(t, b.ID, parsed.String(), "stored in canonical form")
			assert.False(t, seen[b.ID], "id %q reused", b.ID)
			seen[b.ID] = true
		}
	}
	require.GreaterOrEqual(t, count, 3, "the turn, the reply, the tool result")
}

// A caller may pass a bare literal for its turn; Execute gives it an id.
func TestABareTurnIsIdentified(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("hello")}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "atlas"}).WithLLM(llm)

	in := &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-bare",
		Message: history.Message{Messages: []responses.InputMessageUnion{
			responses.UserMessage("hi"),
		}},
	}
	require.Empty(t, in.Message.ID)

	requireStatus(t, runAgent(t, agent, in), agentstate.RunStatusCompleted)
	assert.NotEmpty(t, in.Message.ID, "Execute mints the id before dispatching")
}

// A caller that supplies its own id keeps it.
func TestASuppliedTurnIDIsKept(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("hello")}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "atlas"}).WithLLM(llm)

	msg := userMessage("hi")
	msg.ID = "msg_from_the_queue"

	in := &agents.AgentInput{Namespace: "test", ThreadID: "thread-kept", Message: msg}
	requireStatus(t, runAgent(t, agent, in), agentstate.RunStatusCompleted)

	assert.Equal(t, "msg_from_the_queue", in.Message.ID)
}

// The turn is timed as a whole and the span is stored on the run.
func TestTheRunRecordsTheTurnsSpan(t *testing.T) {
	llm := &scriptedLLM{script: []*responses.Response{textResponse("hello")}}
	agent := agents.NewAgent(&agents.AgentOptions{Name: "atlas"}).WithLLM(llm)

	before := time.Now().Add(-time.Second)
	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-span",
		Message:   userMessage("hi"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	meta := storedRuns(t, agent, "test", "thread-span", out.RunID)[0].Meta

	started, err := time.Parse(time.RFC3339Nano, meta[agentstate.StartedAtMetaKey].(string))
	require.NoError(t, err)
	completed, err := time.Parse(time.RFC3339Nano, meta[agentstate.CompletedAtMetaKey].(string))
	require.NoError(t, err)

	assert.True(t, started.After(before))
	assert.False(t, completed.Before(started), "the agent cannot answer before it was asked")
}

// The turn's span comes from the persistence adapter's clock, which is what a
// durable runtime wraps so a replay is handed the reading it took the first
// time. A clock that never moves proves nothing else is being read.
type frozenClock struct {
	*history.InMemoryConversationPersistence
	at time.Time
}

func (c frozenClock) Now(context.Context) time.Time { return c.at }

func TestTheTurnsSpanComesFromTheAdaptersClock(t *testing.T) {
	frozen := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := frozenClock{InMemoryConversationPersistence: history.NewInMemoryConversationPersistence(), at: frozen}

	llm := &scriptedLLM{script: []*responses.Response{textResponse("hello")}}
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:    "atlas",
		History: history.NewConversationManager(store),
	}).WithLLM(llm)

	out := runAgent(t, agent, &agents.AgentInput{
		Namespace: "test",
		ThreadID:  "thread-frozen",
		Message:   userMessage("hi"),
	})
	requireStatus(t, out, agentstate.RunStatusCompleted)

	meta := storedRuns(t, agent, "test", "thread-frozen", out.RunID)[0].Meta
	assert.Equal(t, frozen.Format(time.RFC3339Nano), meta[agentstate.StartedAtMetaKey])
	assert.Equal(t, frozen.Format(time.RFC3339Nano), meta[agentstate.CompletedAtMetaKey])
}
