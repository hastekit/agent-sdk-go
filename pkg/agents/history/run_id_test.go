package history

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/stretchr/testify/require"
)

func TestCallerRunIDAndApprovalContinuation(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"memory", "file"} {
		t.Run(kind, func(t *testing.T) {
			var p ConversationPersistenceAdapter = NewInMemoryConversationPersistence()
			if kind == "file" {
				file, err := NewFileConversationPersistence(t.TempDir())
				require.NoError(t, err)
				defer file.Close()
				p = file
			}
			cm := NewConversationManager(p)
			first, err := NewRun(ctx, cm, "ns", "thread", "", WithRunID("client-first"))
			require.NoError(t, err)
			require.Equal(t, "client-first", first.GetRunID())
			first.AddMessages(ctx, userBundle("user", "hello"))
			first.RunState.CurrentStep = agentstate.StepAwaitApproval
			first.RunState.LoopIteration = 3
			require.NoError(t, first.SaveMessages(ctx))
			// A new client invocation continues pending state under its own run identity.
			resumed, err := NewRun(ctx, cm, "ns", "thread", "", WithRunID("client-resume"))
			require.NoError(t, err)
			require.Equal(t, "client-resume", resumed.GetRunID())
			require.True(t, resumed.RunState.IsPaused())
			require.Equal(t, 3, resumed.RunState.LoopIteration)
			resumed.AddMessages(ctx, userBundle("user", "approved"))
			resumed.RunState.CurrentStep = agentstate.StepComplete
			require.NoError(t, resumed.SaveMessages(ctx))
			rows, err := cm.LoadTranscript(ctx, "ns", "thread")
			require.NoError(t, err)
			require.Len(t, rows, 2)
			require.Equal(t, "client-first", rows[0].RunID)
			require.Equal(t, "client-resume", rows[1].RunID)
			require.Len(t, rows[0].Messages, 1)
			require.Len(t, rows[1].Messages, 1)
			generated, err := NewRun(ctx, cm, "ns", "thread", "")
			require.NoError(t, err)
			require.NotEmpty(t, generated.GetRunID())
			require.NotEqual(t, "client-resume", generated.GetRunID())
			// Another namespace can use the same ID.
			other, err := NewRun(ctx, cm, "other", "thread", "", WithRunID("client-first"))
			require.NoError(t, err)
			other.AddMessages(ctx, userBundle("user", "other tenant"))
			require.NoError(t, other.SaveMessages(ctx))
			// A namespace-wide ID must not append into a different thread's history.
			conflict, err := NewRun(ctx, cm, "ns", "different-thread", "", WithRunID("client-first"))
			require.NoError(t, err)
			conflict.AddMessages(ctx, userBundle("user", "wrong thread"))
			require.ErrorContains(t, conflict.SaveMessages(ctx), "another thread")
		})
	}
}

// A caller-supplied ID must not trigger a second history query.
type countedRunReads struct {
	*InMemoryConversationPersistence
	loads, transcripts int
}

func (p *countedRunReads) LoadMessages(ctx context.Context, ns, thread, previous string) ([]ConversationMessage, error) {
	p.loads++
	return p.InMemoryConversationPersistence.LoadMessages(ctx, ns, thread, previous)
}
func (p *countedRunReads) LoadTranscript(ctx context.Context, ns, thread string) ([]ConversationMessage, error) {
	p.transcripts++
	return p.InMemoryConversationPersistence.LoadTranscript(ctx, ns, thread)
}
func TestCallerRunIDDoesNotReadTranscript(t *testing.T) {
	p := &countedRunReads{InMemoryConversationPersistence: NewInMemoryConversationPersistence()}
	_, err := NewRun(context.Background(), NewConversationManager(p), "ns", "thread", "", WithRunID("client"))
	require.NoError(t, err)
	require.Equal(t, 1, p.loads)
	require.Zero(t, p.transcripts)
}
