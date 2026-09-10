package agents

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/stretchr/testify/require"
)

type waitingForHeadersProvider struct {
	base.BaseProvider
	entered chan struct{}
}

func (p *waitingForHeadersProvider) NewStreamingResponses(ctx context.Context, _ *responses.Request) (chan *responses.ResponseChunk, error) {
	close(p.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestWrappedLLMStopBeforeStreamStarts(t *testing.T) {
	p := &waitingForHeadersProvider{entered: make(chan struct{})}
	llm := &WrappedLLM{llm: p}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := llm.NewStreamingResponses(ctx, &ModelCall{}, &responses.Request{}, func(*responses.ResponseChunk) {})
		done <- err
	}()
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrModelCallStopped)
	case <-time.After(time.Second):
		t.Fatal("model call did not stop")
	}
}
