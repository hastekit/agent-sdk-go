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
func TestInputMessageChunkRoundTrips(t *testing.T) {
	data, err := sonic.Marshal(&ResponseChunk{
		OfInputMessage: &ChunkInputMessage[constants.ChunkTypeInputMessage]{
			MessageID: "msg_1", Role: "user", Content: "hurry up", SenderID: "alice",
		},
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type": "input_message",
		"message_id": "msg_1",
		"role": "user",
		"content": "hurry up",
		"sender_id": "alice"
	}`, string(data))

	var out ResponseChunk
	require.NoError(t, sonic.Unmarshal(data, &out))
	require.NotNil(t, out.OfInputMessage)
	assert.Equal(t, "msg_1", out.OfInputMessage.MessageID,
		"the id is what tells the client that sent this turn that it is its own echo")
	assert.Equal(t, "hurry up", out.OfInputMessage.Content)
	assert.Equal(t, "alice", out.OfInputMessage.SenderID)
}

func TestInputMessageChunkType(t *testing.T) {
	chunk := &ResponseChunk{
		OfInputMessage: &ChunkInputMessage[constants.ChunkTypeInputMessage]{MessageID: "msg_1"},
	}
	assert.Equal(t, "input_message", chunk.ChunkType())
}

// A thread with one participant sends no sender, and the field goes away
// rather than travelling as an empty string.
func TestInputMessageChunkOmitsAnEmptySender(t *testing.T) {
	data, err := sonic.Marshal(&ResponseChunk{
		OfInputMessage: &ChunkInputMessage[constants.ChunkTypeInputMessage]{
			MessageID: "msg_1", Role: "user", Content: "hi",
		},
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"input_message","message_id":"msg_1","role":"user","content":"hi"}`, string(data))
}
