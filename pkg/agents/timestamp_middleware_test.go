package agents

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestTimestampMiddlewareStampsModelReplies(t *testing.T) {
	original := &responses.Response{}
	call := TimestampMiddleware{}.WrapModelCall(func(context.Context, *ModelCall, *responses.Request) (*responses.Response, error) {
		return original, nil
	})

	before := time.Now()
	got, err := call(context.Background(), &ModelCall{}, &responses.Request{})
	require.NoError(t, err)
	at, ok, err := got.CreatedAt()
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, at.Before(before))
	require.Nil(t, original.Metadata, "the reply it was handed is left as it was")
}

func TestTimestampMiddlewareStampsToolResults(t *testing.T) {
	const working = 30 * time.Millisecond
	original := &ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		CallID: "call_1", Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("ok")},
	}}
	call := TimestampMiddleware{}.WrapToolCall(func(context.Context, *BaseTool, *ToolCall) (*ToolCallResponse, error) {
		time.Sleep(working)
		return original, nil
	})

	started := time.Now()
	got, err := call(context.Background(), &BaseTool{}, &ToolCall{})
	require.NoError(t, err)
	at, ok, err := got.CreatedAt()
	require.NoError(t, err)
	require.True(t, ok)
	require.GreaterOrEqual(t, at.Sub(started), working, "stamped when the result came back")
	require.Nil(t, original.Metadata, "the result it was handed is left as it was")

	// The same reading survives the JSON a durable runtime journals.
	data, err := json.Marshal(got)
	require.NoError(t, err)
	var crossed ToolCallResponse
	require.NoError(t, json.Unmarshal(data, &crossed))
	again, ok, err := crossed.CreatedAt()
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, at.Equal(again))
}
