package streambroker

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type turnRouter interface {
	EnqueueOrStart(ctx context.Context, channel string, msgs []messages.Message) (bool, error)
	WaitToolResult(ctx context.Context, channel, callID string, timeout time.Duration) (string, bool, error)
	DrainMessages(ctx context.Context, channel string) ([]messages.Message, error)
	Publish(ctx context.Context, channel string, chunk *responses.ResponseChunk) error
	Close(ctx context.Context, channel string) error
}

func routers(t *testing.T) map[string]func(*testing.T) turnRouter {
	return map[string]func(*testing.T) turnRouter{
		"memory": func(*testing.T) turnRouter { return NewMemoryStreamBroker() },
		"redis":  func(t *testing.T) turnRouter { broker, _ := testRedisBroker(t); return broker },
	}
}

func toolOutputTurn(callID, output string) messages.Message {
	return messages.Message{Messages: []responses.InputMessageUnion{{
		OfFunctionCallOutput: &responses.FunctionCallOutputMessage{
			CallID: callID,
			Output: responses.FunctionCallOutputContentUnion{OfString: &output},
		},
	}}}
}

func userTurn(text string) messages.Message {
	return messages.Message{Messages: []responses.InputMessageUnion{{
		OfInputMessage: &responses.InputMessage{Role: constants.RoleUser, Content: responses.InputContent{
			{OfInputText: &responses.InputTextContent{Text: text}},
		}},
	}}}
}

func TestLiveRunTakesToolOutputsPerCall(t *testing.T) {
	for name, newBroker := range routers(t) {
		t.Run(name, func(t *testing.T) {
			broker := newBroker(t)
			ctx := t.Context()
			channel := "thread-" + name
			started, err := broker.EnqueueOrStart(ctx, channel, []messages.Message{userTurn("hello")})
			require.NoError(t, err)
			require.True(t, started)

			// A waiter started before delivery wakes when the output arrives.
			got := make(chan string, 1)
			go func() {
				result, found, err := broker.WaitToolResult(ctx, channel, "call-1", 5*time.Second)
				if err == nil && found {
					got <- result
				}
			}()
			time.Sleep(50 * time.Millisecond)

			// Outputs go to their calls; anything else in the turn is queued for the loop.
			mixed := userTurn("also this")
			mixed.Messages = append(toolOutputTurn("call-1", "first").Messages, mixed.Messages...)
			started, err = broker.EnqueueOrStart(ctx, channel, []messages.Message{mixed})
			require.NoError(t, err)
			require.False(t, started)
			select {
			case result := <-got:
				require.Equal(t, "first", result)
			case <-time.After(2 * time.Second):
				t.Fatal("waiter was not woken by the output")
			}
			queued, err := broker.DrainMessages(ctx, channel)
			require.NoError(t, err)
			require.Len(t, queued, 1)
			require.Len(t, queued[0].Messages, 1)
			// A queued message may come back as either input shape; it is the text, not the output.
			require.Nil(t, queued[0].Messages[0].OfFunctionCallOutput)

			// The first output wins, and a later read (a retried activity) sees it,
			// even with no time to wait.
			_, err = broker.EnqueueOrStart(ctx, channel, []messages.Message{toolOutputTurn("call-1", "second")})
			require.NoError(t, err)
			result, found, err := broker.WaitToolResult(ctx, channel, "call-1", 0)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "first", result)
			queued, err = broker.DrainMessages(ctx, channel)
			require.NoError(t, err)
			require.Empty(t, queued, "a duplicate output is not queued for the loop")

			// Nothing delivered: the wait ends at its timeout, not in an error.
			_, found, err = broker.WaitToolResult(ctx, channel, "never", 30*time.Millisecond)
			require.NoError(t, err)
			require.False(t, found)

			// A caller that goes away is an error, not a timeout.
			gone, cancel := context.WithCancel(ctx)
			cancel()
			_, _, err = broker.WaitToolResult(gone, channel, "never", time.Minute)
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestTurnWaitsOutAFinishingRun(t *testing.T) {
	for name, newBroker := range routers(t) {
		t.Run(name, func(t *testing.T) {
			broker := newBroker(t)
			ctx := t.Context()
			channel := "finishing-" + name
			started, err := broker.EnqueueOrStart(ctx, channel, []messages.Message{userTurn("hello")})
			require.NoError(t, err)
			require.True(t, started)

			// The run publishes its end; the claim is released only after it is saved.
			require.NoError(t, broker.Publish(ctx, channel, &responses.ResponseChunk{
				OfRunPaused: &responses.ChunkRun[constants.ChunkTypeRunPaused]{},
			}))
			routed := make(chan bool, 1)
			go func() {
				started, err := broker.EnqueueOrStart(ctx, channel, []messages.Message{toolOutputTurn("call-1", "answer")})
				if err == nil {
					routed <- started
				}
			}()
			select {
			case <-routed:
				t.Fatal("a turn joined a run that had already ended")
			case <-time.After(150 * time.Millisecond):
			}

			// Once released, the turn starts the next run instead of being lost.
			require.NoError(t, broker.Close(ctx, channel))
			select {
			case started := <-routed:
				require.True(t, started)
			case <-time.After(2 * time.Second):
				t.Fatal("the turn did not start a run after the release")
			}
		})
	}
}

func TestSplitToolOutputs(t *testing.T) {
	mixed := userTurn("hi")
	mixed.Messages = append(mixed.Messages, toolOutputTurn("c2", "two").Messages...)
	outputs, rest := splitToolOutputs([]messages.Message{toolOutputTurn("c1", "one"), mixed})
	require.Equal(t, []toolOutput{{callID: "c1", content: "one"}, {callID: "c2", content: "two"}}, outputs)
	require.Len(t, rest, 1)
	require.Len(t, rest[0].Messages, 1)
	require.NotNil(t, rest[0].Messages[0].OfInputMessage)
}
