package restate_runtime

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/stretchr/testify/require"
)

// Observe both ends of the lifecycle on the real broker.
type heartbeatTrackingBroker struct {
	*streambroker.MemoryStreamBroker
	ctx     context.Context
	channel string
	stopped bool
}

func (b *heartbeatTrackingBroker) StartHeartbeat(ctx context.Context, channel string) func() {
	b.ctx = ctx
	b.channel = channel
	return func() { b.stopped = true }
}

// A nil Restate context proves lifecycle forwarding does not invoke durable APIs.
func TestRestateHeartbeatForwardsWithoutDurability(t *testing.T) {
	broker := &heartbeatTrackingBroker{MemoryStreamBroker: streambroker.NewMemoryStreamBroker()}
	proxy := NewRestateStreamBroker(nil, broker)
	ctx := t.Context()

	// Forward the execution context and stream ID without inspecting broker capabilities.
	stop := proxy.StartHeartbeat(ctx, "stream")
	require.Same(t, ctx, broker.ctx)
	require.Equal(t, "stream", broker.channel)
	require.False(t, broker.stopped)

	// Return the wrapped broker's cleanup function to the execution owner.
	stop()
	stop()
	require.True(t, broker.stopped)
}
