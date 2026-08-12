package history

import (
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A durable runtime moves thread meta across a serialization boundary on
// every load and save: Temporal's LoadMessagesActivity returns it through the
// data converter, and Restate's run step does the same. What goes in as a
// ToolPermissions struct therefore comes back as a plain map, so the reader
// has to accept both shapes — in-process it never left the process, and under
// a durable runtime it always has.
func TestToolPermissions_SurviveTheDurableBoundary(t *testing.T) {
	written := map[string]any{
		ToolPermissionsMetaKey: ToolPermissions{
			AlwaysAllow: []string{"search"},
			AlwaysDeny:  []string{"send_email"},
		},
	}

	// In-process: the value in the map is still the struct.
	direct := toolPermissionsFromMeta(written)
	assert.Equal(t, []string{"search"}, direct.AlwaysAllow)
	assert.Equal(t, []string{"send_email"}, direct.AlwaysDeny)

	// Through an activity boundary: struct → JSON → map[string]any.
	encoded, err := sonic.Marshal(written)
	require.NoError(t, err)
	var roundTripped map[string]any
	require.NoError(t, sonic.Unmarshal(encoded, &roundTripped))
	require.IsType(t, map[string]any{}, roundTripped[ToolPermissionsMetaKey],
		"the boundary should have turned the struct into a map — otherwise this test proves nothing")

	crossed := toolPermissionsFromMeta(roundTripped)
	assert.Equal(t, []string{"search"}, crossed.AlwaysAllow)
	assert.Equal(t, []string{"send_email"}, crossed.AlwaysDeny)
}

// The run manager writes permissions into the same meta map that carries run
// state, so a durable runtime's save/load cycle has to preserve both.
func TestToolPermissions_ShareMetaWithRunState(t *testing.T) {
	ctx := t.Context()
	cm := NewConversationManager(NewInMemoryConversationPersistence())

	run, err := NewRun(ctx, cm, "ns", "thread-1", "")
	require.NoError(t, err)
	run.UpdateToolPermissions(func(p *ToolPermissions) { p.Deny("send_email") })
	run.AddMessages(ctx, permissionsTestTurn("hello"))
	require.NoError(t, run.SaveMessages(ctx))

	// Round-trip the saved meta the way an activity would, then reload from it.
	_, meta, err := cm.lastTurn(ctx, "ns", "thread-1")
	require.NoError(t, err)

	encoded, err := sonic.Marshal(meta)
	require.NoError(t, err)
	var crossed map[string]any
	require.NoError(t, sonic.Unmarshal(encoded, &crossed))

	assert.Equal(t, []string{"send_email"}, toolPermissionsFromMeta(crossed).AlwaysDeny)
	assert.Contains(t, crossed, "run_state", "run state must still be there beside the permissions")
}
