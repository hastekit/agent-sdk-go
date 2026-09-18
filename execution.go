package sdk

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

type Input = agents.Input
type Message = responses.InputMessageUnion
type Result = agents.AgentOutput
type RunHandle = agents.AgentHandle

var ErrStreamOverflow = agents.ErrStreamOverflow

// User constructs a user text message. Rich messages remain available through Message.
func User(text string) Message { return responses.UserMessage(text) }

// UserTurn creates a user message bundle for Input.Message.
func UserTurn(text string) history.Message { return history.Message{Messages: []Message{User(text)}} }
