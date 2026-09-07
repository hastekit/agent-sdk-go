package temporal_runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The loop reaches the run feed by asking whether its broker is an
// agents.RunFeed. Inside a workflow that broker is a proxy, and a proxy that
// merely holds a feed answers no — so the feed went silent under Temporal
// with nothing to say why.
func TestStreamBrokerProxy_OffersTheRunFeed(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()

	// No workflow context needed: nothing here touches the activity-backed
	// methods, only the ones that forward.
	proxy := temporal_runtime.NewTemporalStreamBrokerProxy(nil, "Agent", broker)

	feed, ok := proxy.(agents.RunFeed)
	require.True(t, ok, "a capability the workflow-side broker does not offer is one the loop cannot use")

	// A cursor first: an empty one means "from now", so an event published
	// before the read would be filtered out as history.
	_, cursor, err := broker.ReadRunEvents(context.Background(), []string{"ns"}, "", 0)
	require.NoError(t, err)

	require.NoError(t, feed.PublishRunEvent(context.Background(), agents.RunEvent{
		Event:     agents.RunEventStarted,
		Namespace: "ns",
		ThreadID:  "thread-1",
		RunID:     "run-1",
		StreamID:  "stream-1",
	}))

	// Read it back off the real broker, which is where a watching browser
	// reads it from — the proxy is only how the workflow reaches it.
	events, _, err := broker.ReadRunEvents(context.Background(), []string{"ns"}, cursor, 0)
	require.NoError(t, err)
	require.Len(t, events, 1, "the event has to reach the broker the feed endpoint reads")
	assert.Equal(t, "thread-1", events[0].ThreadID)
}

// And reading forwards too, so a handler holding the proxy sees the same feed.
func TestStreamBrokerProxy_ForwardsFeedReads(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()
	proxy := temporal_runtime.NewTemporalStreamBrokerProxy(nil, "Agent", broker).(agents.RunFeed)

	cursor := ""
	_, cursor, err := proxy.ReadRunEvents(context.Background(), []string{"ns"}, cursor, 0)
	require.NoError(t, err)

	require.NoError(t, broker.PublishRunEvent(context.Background(), agents.RunEvent{
		Event: agents.RunEventFinished, Namespace: "ns", ThreadID: "thread-1",
	}))

	events, _, err := proxy.ReadRunEvents(context.Background(), []string{"ns"}, cursor, time.Second)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, agents.RunEventFinished, events[0].Event)
}
