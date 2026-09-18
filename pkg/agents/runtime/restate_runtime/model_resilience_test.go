package restate_runtime

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	restate "github.com/restatedev/sdk-go"
	"github.com/stretchr/testify/require"
)

type resilienceProvider struct {
	llm.Provider
	calls     int
	committed bool
}

func (p *resilienceProvider) NewStreamingResponses(context.Context, *responses.Request) (chan *responses.ResponseChunk, error) {
	p.calls++
	if !p.committed {
		return nil, io.ErrUnexpectedEOF
	}
	ch := make(chan *responses.ResponseChunk, 1)
	ch <- &responses.ResponseChunk{OfResponseCreated: &responses.ChunkResponse[constants.ChunkTypeResponseCreated]{}}
	close(ch)
	return ch, nil
}
func TestModelResilienceReturnsTerminalStepOutcome(t *testing.T) {
	for _, committed := range []bool{false, true} {
		p := &resilienceProvider{committed: committed}
		worker := NewRestateLLM(nil, p, "", nil, "stream", agentmiddleware.NewRetry(agentmiddleware.RetryConfig{MaxAttempts: 2, InitialBackoff: time.Nanosecond})).(*RestateLLM)
		_, err := worker.invoke(t.Context(), &agents.ModelCall{}, &responses.Request{}, func(*responses.ResponseChunk) {})
		require.Error(t, err)
		require.EqualValues(t, 597, restate.ErrorCode(err))
		require.True(t, restate.IsTerminalError(err))
		if committed {
			require.Equal(t, 1, p.calls)
		} else {
			require.Equal(t, 2, p.calls)
		}
	}
}
