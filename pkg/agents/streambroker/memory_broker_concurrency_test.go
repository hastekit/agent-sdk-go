package streambroker_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func TestMemoryBrokerCloseReleasesBlockedPublishers(t *testing.T) {
	for _, action := range []string{"cancel", "close", "reset"} {
		t.Run(action, func(t *testing.T) {
			b := streambroker.NewMemoryStreamBroker()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := b.Subscribe(ctx, "run")
			require.NoError(t, err)
			for i := 0; i < 100; i++ {
				require.NoError(t, b.Publish(context.Background(), "run", &responses.ResponseChunk{}))
			}
			entered := make(chan struct{}, 8)
			pubCtx := publishContext{Context: context.Background(), entered: entered}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); _ = b.Publish(pubCtx, "run", &responses.ResponseChunk{}) }()
			}
			for i := 0; i < 8; i++ {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("publisher did not reach blocked send")
				}
			}
			switch action {
			case "cancel":
				cancel()
			case "close":
				require.NoError(t, b.Close(context.Background(), "run"))
			case "reset":
				b.Reset()
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("publisher remained blocked after subscriber closed")
			}
			for range ch {
			}
		})
	}
}

func TestMemoryBrokerConcurrentPublishAndClose(t *testing.T) {
	for i := 0; i < 100; i++ {
		b := streambroker.NewMemoryStreamBroker()
		ctx, cancel := context.WithCancel(context.Background())
		ch, err := b.Subscribe(ctx, "run")
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = b.Publish(ctx, "run", &responses.ResponseChunk{})
			}
		}()
		go func() { defer wg.Done(); _ = b.Close(ctx, "run"); cancel() }()
		for range ch {
		}
		wg.Wait()
	}
}

// Done is evaluated when Publish reaches its send select. Waiting for it
// ensures closure races with captured subscribers, not just a fresh lookup.
type publishContext struct {
	context.Context
	entered chan struct{}
}

func (c publishContext) Done() <-chan struct{} {
	c.entered <- struct{}{}
	return c.Context.Done()
}
