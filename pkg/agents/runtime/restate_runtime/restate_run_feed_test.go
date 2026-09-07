package restate_runtime

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Same as the Temporal proxy: the loop reaches the run feed by asking whether
// its broker is an agents.RunFeed, and inside a workflow that broker is this.
// A wrapper that merely holds a feed answers no, and the feed goes silent.
func TestRestateStreamBroker_OffersTheRunFeed(t *testing.T) {
	broker := streambroker.NewMemoryStreamBroker()

	// No restate context needed: nothing here touches the step-backed methods.
	wrapped := NewRestateStreamBroker(nil, broker)

	feed, ok := any(wrapped).(agents.RunFeed)
	require.True(t, ok, "a capability the workflow-side broker does not offer is one the loop cannot use")

	// An empty cursor means "from now", so take one before publishing.
	_, cursor, err := broker.ReadRunEvents(context.Background(), []string{"ns"}, "", 0)
	require.NoError(t, err)

	require.NoError(t, feed.PublishRunEvent(context.Background(), agents.RunEvent{
		Event:     agents.RunEventStarted,
		Namespace: "ns",
		ThreadID:  "thread-1",
	}))

	events, _, err := broker.ReadRunEvents(context.Background(), []string{"ns"}, cursor, 0)
	require.NoError(t, err)
	require.Len(t, events, 1, "the event has to reach the broker the feed endpoint reads")
	assert.Equal(t, "thread-1", events[0].ThreadID)
}
