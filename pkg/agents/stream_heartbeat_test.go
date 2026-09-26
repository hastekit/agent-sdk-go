package agents

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/stretchr/testify/require"
)

// Observe the ordering between heartbeat cleanup and broker closure.
type closingHeartbeatBroker struct {
	*streambroker.MemoryStreamBroker
	events *[]string
}

func (b *closingHeartbeatBroker) StartHeartbeat(_ context.Context, channel string) func() {
	*b.events = append(*b.events, "start:"+channel)
	return func() { *b.events = append(*b.events, "stop") }
}

func (b *closingHeartbeatBroker) Close(ctx context.Context, channel string) error {
	*b.events = append(*b.events, "close")
	return b.MemoryStreamBroker.Close(ctx, channel)
}

// Fail before generation to exercise cleanup on the earliest loop error.
type failingHeartbeatHistory struct {
	*history.InMemoryConversationPersistence
}

func (*failingHeartbeatHistory) LoadMessages(context.Context, string, string, string) ([]history.ConversationMessage, error) {
	return nil, errors.New("history unavailable")
}

// The same loop setup calls the broker once and stops before closing, even on startup failure.
func TestExecuteLocalOwnsHeartbeatSetup(t *testing.T) {
	var events []string
	broker := &closingHeartbeatBroker{MemoryStreamBroker: streambroker.NewMemoryStreamBroker(), events: &events}
	agent := NewAgent(&AgentOptions{
		StreamBroker: broker,
		History:      history.NewConversationManager(&failingHeartbeatHistory{history.NewInMemoryConversationPersistence()}),
	})

	// An agent without an explicit runtime still starts its broker heartbeat exactly once.
	_, err := agent.ExecuteWithoutTrace(t.Context(), &AgentInput{StreamID: "s", ThreadID: "thread"})
	require.ErrorContains(t, err, "history unavailable")
	require.Equal(t, []string{"start:s", "stop", "close"}, events)
}
