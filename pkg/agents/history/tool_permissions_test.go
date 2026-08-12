package history

import (
	"context"
	"testing"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func permissionsTestTurn(text string) Message {
	return messages.New("user", []responses.InputMessageUnion{{
		OfEasyInput: &responses.EasyMessage{
			Role:    constants.RoleUser,
			Content: responses.EasyInputContentUnion{OfString: utils.Ptr(text)},
		},
	}})
}

// openThread starts a thread with one saved turn, which is what a standing
// decision attaches to.
func openThread(t *testing.T, threadID string) *CommonConversationManager {
	t.Helper()

	cm := NewConversationManager(NewInMemoryConversationPersistence())
	run, err := NewRun(t.Context(), cm, "test", threadID, "")
	require.NoError(t, err)
	run.AddMessages(t.Context(), permissionsTestTurn("hello"))
	require.NoError(t, run.SaveMessages(t.Context()))

	return cm
}

// A decision written from outside a run is there for the next reader.
func TestThreadToolPermissions_RoundTrip(t *testing.T) {
	cm := openThread(t, "thread-1")

	require.NoError(t, cm.UpdateThreadToolPermissions(t.Context(), "test", "thread-1", func(p *ToolPermissions) {
		p.Allow("search")
		p.Deny("send_email")
	}))

	got, err := cm.ThreadToolPermissions(t.Context(), "test", "thread-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"search"}, got.AlwaysAllow)
	assert.Equal(t, []string{"send_email"}, got.AlwaysDeny)
}

// A run reads the thread's decisions when it loads, and writes them back when
// it saves — otherwise meta, which is rebuilt from run state each turn, would
// drop them at the end of the turn that recorded them.
func TestThreadToolPermissions_SurviveTheNextRun(t *testing.T) {
	ctx := context.Background()
	cm := openThread(t, "thread-2")

	require.NoError(t, cm.UpdateThreadToolPermissions(ctx, "test", "thread-2", func(p *ToolPermissions) {
		p.Deny("send_email")
	}))

	// A run that knows nothing about permissions still has to carry them.
	run, err := NewRun(ctx, cm, "test", "thread-2", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"send_email"}, run.ToolPermissions().AlwaysDeny)
	run.AddMessages(ctx, permissionsTestTurn("another turn"))
	require.NoError(t, run.SaveMessages(ctx))

	got, err := cm.ThreadToolPermissions(ctx, "test", "thread-2")
	require.NoError(t, err)
	assert.Equal(t, []string{"send_email"}, got.AlwaysDeny)
}

// A decision recorded during a run persists with that run's save.
func TestToolPermissions_UpdatedWithinARun(t *testing.T) {
	ctx := context.Background()
	cm := openThread(t, "thread-3")

	run, err := NewRun(ctx, cm, "test", "thread-3", "")
	require.NoError(t, err)
	run.UpdateToolPermissions(func(p *ToolPermissions) { p.Allow("search") })
	run.AddMessages(ctx, permissionsTestTurn("a turn"))
	require.NoError(t, run.SaveMessages(ctx))

	got, err := cm.ThreadToolPermissions(ctx, "test", "thread-3")
	require.NoError(t, err)
	assert.Equal(t, []string{"search"}, got.AlwaysAllow)
}

// Clearing the last decision has to remove the entry, not save an empty one
// that reads back as a decision nobody made.
func TestToolPermissions_ClearRemovesTheEntry(t *testing.T) {
	ctx := context.Background()
	cm := openThread(t, "thread-4")

	require.NoError(t, cm.UpdateThreadToolPermissions(ctx, "test", "thread-4", func(p *ToolPermissions) {
		p.Allow("search")
	}))
	require.NoError(t, cm.UpdateThreadToolPermissions(ctx, "test", "thread-4", func(p *ToolPermissions) {
		p.Clear("search")
	}))

	got, err := cm.ThreadToolPermissions(ctx, "test", "thread-4")
	require.NoError(t, err)
	assert.True(t, got.IsEmpty())
}

// Deciding the other way has to take the name off the first list, or the deny
// — which wins — would be permanent.
func TestToolPermissions_ReversalMovesTheName(t *testing.T) {
	var p ToolPermissions

	p.Allow("risky")
	p.Deny("risky")
	assert.Empty(t, p.AlwaysAllow)
	assert.Equal(t, []string{"risky"}, p.AlwaysDeny)

	p.Allow("risky")
	assert.Equal(t, []string{"risky"}, p.AlwaysAllow)
	assert.Empty(t, p.AlwaysDeny)
}

// Recording the same decision twice leaves one entry.
func TestToolPermissions_NoDuplicates(t *testing.T) {
	var p ToolPermissions

	p.Allow("search", "search")
	p.Allow("search")
	assert.Equal(t, []string{"search"}, p.AlwaysAllow)
}

// A thread with nothing in it has nowhere to put a decision, and says so
// rather than dropping it.
func TestThreadToolPermissions_EmptyThread(t *testing.T) {
	cm := NewConversationManager(NewInMemoryConversationPersistence())

	err := cm.UpdateThreadToolPermissions(t.Context(), "test", "nonexistent", func(p *ToolPermissions) {
		p.Deny("anything")
	})
	assert.Error(t, err)
}
