package responses

import (
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A chunk union marshals to the flat chunk, not to a wrapper naming the arm.
// Getting this wrong is invisible until something on the far side of a broker
// cannot read what it was sent.
func TestBackgroundTaskStartedRoundTrips(t *testing.T) {
	data, err := sonic.Marshal(&ResponseChunk{
		OfBackgroundTaskStarted: &ChunkBackgroundTask[constants.ChunkTypeBackgroundTaskStarted]{
			TaskID: "task-1", CallID: "call_1", ToolName: "index", StreamID: "task-stream",
		},
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type": "background_task.started",
		"task_id": "task-1",
		"call_id": "call_1",
		"tool_name": "index",
		"stream_id": "task-stream"
	}`, string(data))

	var out ResponseChunk
	require.NoError(t, sonic.Unmarshal(data, &out))
	require.NotNil(t, out.OfBackgroundTaskStarted)
	assert.Equal(t, "call_1", out.OfBackgroundTaskStarted.CallID,
		"the call the task belongs to, so a client updates the row it already drew")
	assert.Equal(t, "task-stream", out.OfBackgroundTaskStarted.StreamID,
		"the task's own channel, which is where its progress goes")
}

func TestBackgroundTaskCompletedRoundTrips(t *testing.T) {
	data, err := sonic.Marshal(&ResponseChunk{
		OfBackgroundTaskCompleted: &ChunkBackgroundTask[constants.ChunkTypeBackgroundTaskCompleted]{
			TaskID: "task-1", CallID: "call_1", ToolName: "index", StreamID: "task-stream",
		},
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type": "background_task.completed",
		"task_id": "task-1",
		"call_id": "call_1",
		"tool_name": "index",
		"stream_id": "task-stream"
	}`, string(data))

	var out ResponseChunk
	require.NoError(t, sonic.Unmarshal(data, &out))
	require.NotNil(t, out.OfBackgroundTaskCompleted)
	assert.Nil(t, out.OfBackgroundTaskStarted, "the two are told apart by their type")
}

func TestBackgroundTaskChunkType(t *testing.T) {
	started := &ResponseChunk{OfBackgroundTaskStarted: &ChunkBackgroundTask[constants.ChunkTypeBackgroundTaskStarted]{}}
	completed := &ResponseChunk{OfBackgroundTaskCompleted: &ChunkBackgroundTask[constants.ChunkTypeBackgroundTaskCompleted]{}}

	assert.Equal(t, "background_task.started", started.ChunkType())
	assert.Equal(t, "background_task.completed", completed.ChunkType())
}
