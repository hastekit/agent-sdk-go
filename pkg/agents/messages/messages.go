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

	// BackgroundTaskID names the task whose result this bundle carries.
	//
	// It is what lets a run reconcile the tasks it is still waiting on. The
	// result arrives as an ordinary user turn — the call that started the task
	// was answered when the tool returned, and a provider will not take a
	// second output against it — so a run has no other way to know that the
	// turn it just picked up is a task landing rather than someone speaking.
	//
	// Nothing announces the arrival from here: that is published by the
	// delivery, which knows the call and the tool as well. This is only the
	// bookkeeping.
	BackgroundTaskID string `json:"background_task_id,omitempty" db:"background_task_id"`
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
