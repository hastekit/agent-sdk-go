package messages

import (
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// Message is a bundle of provider messages authored by a single sender.
// It is the unit that carries multi-participant attribution through the
// run state (queued messages), the broker queue, and persistence.
type Message struct {
	ID       string                        `json:"id" db:"id"`
	SenderID string                        `json:"sender_id" db:"sender_id"`
	Messages []responses.InputMessageUnion `json:"messages" db:"messages"`
}

// New builds a bundle under a fresh uuid.
func New(senderID string, messages []responses.InputMessageUnion) Message {
	return NewWithID(uuid.NewString(), senderID, messages)
}

// NewWithID builds a bundle under an id the caller has already minted. The
// agent loop uses it: it runs inside a workflow under the durable runtimes,
// where a uuid drawn on the spot differs on every replay, so the id comes from
// the runtime's own journaled source instead.
func NewWithID(id, senderID string, messages []responses.InputMessageUnion) Message {
	return Message{
		ID:       id,
		SenderID: senderID,
		Messages: messages,
	}
}
