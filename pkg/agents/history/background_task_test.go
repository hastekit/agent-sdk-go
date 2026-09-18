package history

import (
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runManagerWithTasks(taskIDs ...string) *ConversationRunManager {
	cm := &ConversationRunManager{RunState: agentstate.NewRunState()}
	for _, id := range taskIDs {
		cm.RunState.AddBackgroundTask(agentstate.BackgroundTask{
			TaskID: id, ToolName: "index", StartedAt: time.Now().UTC(),
		})
	}
	return cm
}

func notice(text string) []responses.InputMessageUnion {
	return []responses.InputMessageUnion{responses.UserMessage(text)}
}

// A task's result landing is the answer to something the run is carrying, the
// same as an approval is — so it is reconciled where every incoming bundle
// already passes.
func TestProcessIncoming_CompletesTheTaskAResultBelongsTo(t *testing.T) {
	cm := runManagerWithTasks("task-1", "task-2")

	bundle := messages.New("", notice("[Background task task-1 has finished.]"))
	bundle.BackgroundTaskID = "task-1"
	cm.ProcessIncomingMessages(bundle, false)

	remaining := cm.RunState.BackgroundTaskList()
	require.Len(t, remaining, 1, "only the one that landed is dropped")
	assert.Equal(t, "task-2", remaining[0].TaskID)
}

// The queue is the other way a result arrives — a task landing while a run is
// already going folds in there rather than waking one.
func TestProcessIncoming_CompletesFromTheQueueToo(t *testing.T) {
	cm := runManagerWithTasks("task-1")

	bundle := messages.New("", notice("[Background task task-1 has finished.]"))
	bundle.BackgroundTaskID = "task-1"
	cm.ProcessIncomingMessages(bundle, true)

	assert.False(t, cm.RunState.HasBackgroundTasks())
}

// An ordinary turn leaves the list alone. Without the marker there is nothing
// to tell it apart from a task landing — which is the whole reason the marker
// exists.
func TestProcessIncoming_LeavesTasksAloneForAnOrdinaryTurn(t *testing.T) {
	cm := runManagerWithTasks("task-1")

	cm.ProcessIncomingMessages(messages.New("user", notice("what is the weather")), false)

	assert.True(t, cm.RunState.HasBackgroundTasks(),
		"a turn of the user's own is not a task landing")
}

// A result for something this run is not carrying is simply not its business.
func TestProcessIncoming_IgnoresAnUnknownTask(t *testing.T) {
	cm := runManagerWithTasks("task-1")

	bundle := messages.New("", notice("[Background task task-9 has finished.]"))
	bundle.BackgroundTaskID = "task-9"
	assert.NotPanics(t, func() { cm.ProcessIncomingMessages(bundle, false) })

	assert.True(t, cm.RunState.HasBackgroundTasks())
}
