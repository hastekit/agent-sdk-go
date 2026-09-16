package temporal_runtime_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

type resilienceProvider struct {
	llm.Provider
	t         *testing.T
	calls     int
	committed bool
}

func (p *resilienceProvider) NewStreamingResponses(ctx context.Context, _ *responses.Request) (chan *responses.ResponseChunk, error) {
	require.True(p.t, activity.IsActivity(ctx))
	p.calls++
	if !p.committed {
		return nil, io.ErrUnexpectedEOF
	}
	ch := make(chan *responses.ResponseChunk, 1)
	ch <- &responses.ResponseChunk{OfResponseCreated: &responses.ChunkResponse[constants.ChunkTypeResponseCreated]{}}
	close(ch)
	return ch, nil
}
func TestModelResilienceTerminatesInsideTemporalActivity(t *testing.T) {
	for _, committed := range []bool{false, true} {
		p := &resilienceProvider{t: t, committed: committed}
		broker := streambroker.NewMemoryStreamBroker()
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestActivityEnvironment()
		worker := temporal_runtime.NewTemporalLLM(p, broker, agentmiddleware.NewRetry(agentmiddleware.RetryConfig{MaxAttempts: 2, InitialBackoff: time.Nanosecond}))
		env.RegisterActivity(worker.NewStreamingResponsesActivity)
		_, err := env.ExecuteActivity(worker.NewStreamingResponsesActivity, &responses.Request{}, &agents.ModelCall{})
		var appErr *temporal.ApplicationError
		require.True(t, errors.As(err, &appErr))
		require.True(t, appErr.NonRetryable())
		require.Equal(t, "ModelCallFailed", appErr.Type())
		if committed {
			require.Equal(t, 1, p.calls)
		} else {
			require.Equal(t, 2, p.calls)
		}
	}
}
