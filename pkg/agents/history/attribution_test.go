package history

import (
	"strings"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// userMsg builds a simple user-role input message for the tests.
func userMsg(text string) responses.InputMessageUnion {
	return responses.InputMessageUnion{
		OfInputMessage: &responses.InputMessage{
			Role:    constants.RoleUser,
			Content: responses.InputContent{{OfInputText: &responses.InputTextContent{Text: text}}},
		},
	}
}

func firstText(msg responses.InputMessageUnion) string {
	if msg.OfInputMessage == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range msg.OfInputMessage.Content {
		if c.OfInputText != nil {
			b.WriteString(c.OfInputText.Text)
		}
	}
	return b.String()
}

// TestAttributeMessages_DisabledByDefault verifies that with attribution off
// (the default), a message from another sender is passed through untouched.
func TestAttributeMessages_DisabledByDefault(t *testing.T) {
	cm := &ConversationRunManager{}

	bundle := messages.New("other-human", []responses.InputMessageUnion{userMsg("hello")})
	out := cm.attributeMessages([]Message{bundle}, "me")

	if len(out) != 1 {
		t.Fatalf("got %d messages, want 1", len(out))
	}
	if got := firstText(out[0]); got != "hello" {
		t.Fatalf("attribution applied while disabled: %q", got)
	}
}

// TestAttributeMessages_EnabledRewritesOtherSenders verifies that with
// attribution on, a message from another human sender is prefixed.
func TestAttributeMessages_EnabledRewritesOtherSenders(t *testing.T) {
	cm := &ConversationRunManager{messageAttribution: true}

	bundle := messages.New("other-human", []responses.InputMessageUnion{userMsg("hello")})
	out := cm.attributeMessages([]Message{bundle}, "me")

	if len(out) != 1 {
		t.Fatalf("got %d messages, want 1", len(out))
	}
	if got := firstText(out[0]); got != "(Human) other-human said: hello" {
		t.Fatalf("attribution not applied: %q", got)
	}
}

// TestAttributeMessages_EnabledOwnSenderUntouched verifies that even with
// attribution on, the running agent's own messages are left as-is.
func TestAttributeMessages_EnabledOwnSenderUntouched(t *testing.T) {
	cm := &ConversationRunManager{messageAttribution: true}

	bundle := messages.New("me", []responses.InputMessageUnion{userMsg("hello")})
	out := cm.attributeMessages([]Message{bundle}, "me")

	if got := firstText(out[0]); got != "hello" {
		t.Fatalf("own message rewritten: %q", got)
	}
}
