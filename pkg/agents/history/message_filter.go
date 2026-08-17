package history

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
)

// MessageFilter transforms a run's message bundles before they are
// attributed and sent to the provider. It is given the full bundle list
// and the running agent's id, and returns a (possibly rewritten) bundle
// list. Implementations must not mutate the input bundles — return
// copy-on-write clones for anything they change.
//
// The canonical use is rewriting each bundle's SenderID from an opaque id
// to a human-friendly name, so the conversation manager attributes turns
// with names instead of ids.
type MessageFilter interface {
	Filter(ctx context.Context, msgs []messages.Message, agentID string) []messages.Message
}
