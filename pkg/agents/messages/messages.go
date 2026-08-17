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

func New(senderID string, messages []responses.InputMessageUnion) Message {
	return Message{
		ID:       uuid.NewString(),
		SenderID: senderID,
		Messages: messages,
	}
}
