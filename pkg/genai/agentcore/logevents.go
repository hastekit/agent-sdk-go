package agentcore

import (
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/genai"
)

// This file derives the AgentCore-specific message log-event shapes from the
// vendor-neutral semconv message model (genai.Message). AWS CloudWatch GenAI
// Observability renders a span's Input/Output not from the
// gen_ai.input.messages / gen_ai.output.messages span attributes, but from
// separate log records correlated to the span by trace/span id. Two shapes
// were observed on working AgentCore spans:
//
//   - Per-message events (gen_ai.user.message / gen_ai.choice / …), each with
//     its own body.
//   - A single composite gen_ai.client.inference.operation.details event whose
//     body is {"input":{"messages":[...]},"output":{"messages":[...]}}.
//
// The exporter uses the composite (InferenceDetails); the per-message builders
// are retained as the alternative representation. Bodies mirror the AWS Bedrock
// instrumentation exactly (see InferenceDetails).

// LogEvent is one gen_ai.* event: an event name plus a body ready to become an
// OTLP LogRecord body.
type LogEvent struct {
	EventName string
	Body      map[string]any
}

// Event names (OTel GenAI event convention, as used by AWS AgentCore).
const (
	EventSystemMessage    = "gen_ai.system.message"
	EventUserMessage      = "gen_ai.user.message"
	EventAssistantMessage = "gen_ai.assistant.message"
	EventToolMessage      = "gen_ai.tool.message"
	EventChoice           = "gen_ai.choice"
)

// InferenceDetails builds the composite gen_ai inference-details event body —
// {"input":{"messages":[...]},"output":{"messages":[...]}} — that AgentCore's
// GenAI Observability renders a span's Input/Output from. The per-role content
// nesting mirrors the AWS Bedrock instrumentation exactly (system content is a
// plain string; other input messages nest {"content":[{"text":...}]}; output
// content is {"message":<text>,"finish_reason":<reason>}). Returns false when
// there is nothing to record.
func InferenceDetails(inputSemconv, outputSemconv string) (map[string]any, bool) {
	body := map[string]any{}
	if in := inputWireMessages(inputSemconv); len(in) > 0 {
		body["input"] = map[string]any{"messages": in}
	}
	if out := outputWireMessages(outputSemconv); len(out) > 0 {
		body["output"] = map[string]any{"messages": out}
	}
	if len(body) == 0 {
		return nil, false
	}
	return body, true
}

func inputWireMessages(semconvJSON string) []map[string]any {
	msgs, ok := genai.DecodeMessages(semconvJSON)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := orDefault(m.Role, "user")
		if role == "system" {
			// System prompt is carried as a plain string.
			out = append(out, map[string]any{"role": role, "content": joinText(m.Parts)})
			continue
		}
		out = append(out, map[string]any{
			"role":    role,
			"content": map[string]any{"content": textContentArray(m.Parts)},
		})
	}
	return out
}

func outputWireMessages(semconvJSON string) []map[string]any {
	msgs, ok := genai.DecodeMessages(semconvJSON)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"role": orDefault(m.Role, "assistant"),
			"content": map[string]any{
				"message":       joinText(m.Parts),
				"finish_reason": orDefault(m.FinishReason, "stop"),
			},
		})
	}
	return out
}

// InputMessageEvents converts semconv input-message JSON into one gen_ai.*
// message event per message (the alternative to the composite form).
func InputMessageEvents(semconvJSON string) []LogEvent {
	msgs, ok := genai.DecodeMessages(semconvJSON)
	if !ok {
		return nil
	}
	out := make([]LogEvent, 0, len(msgs))
	for _, m := range msgs {
		if ev, ok := inputEvent(m); ok {
			out = append(out, ev)
		}
	}
	return out
}

// OutputMessageEvents converts semconv output-message JSON into gen_ai.choice
// events (one per choice/message).
func OutputMessageEvents(semconvJSON string) []LogEvent {
	msgs, ok := genai.DecodeMessages(semconvJSON)
	if !ok {
		return nil
	}
	out := make([]LogEvent, 0, len(msgs))
	for i, m := range msgs {
		content, toolCalls, _ := splitParts(m.Parts)
		message := map[string]any{"role": orDefault(m.Role, "assistant")}
		if len(content) > 0 {
			message["content"] = content
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		out = append(out, LogEvent{
			EventName: EventChoice,
			Body: map[string]any{
				"message":       message,
				"index":         i,
				"finish_reason": orDefault(m.FinishReason, "stop"),
			},
		})
	}
	return out
}

// SystemMessageEvent builds a gen_ai.system.message event from plain system
// instructions (the gen_ai.system_instructions attribute), for spans that
// carry the system prompt separately from the message list.
func SystemMessageEvent(instructions string) (LogEvent, bool) {
	if instructions == "" {
		return LogEvent{}, false
	}
	return LogEvent{
		EventName: EventSystemMessage,
		Body:      map[string]any{"content": []map[string]any{{"text": instructions}}},
	}, true
}

// HasSystemMessage reports whether any event is a system message, so callers
// can avoid duplicating one derived from gen_ai.system_instructions.
func HasSystemMessage(events []LogEvent) bool {
	for _, e := range events {
		if e.EventName == EventSystemMessage {
			return true
		}
	}
	return false
}

func inputEvent(m genai.Message) (LogEvent, bool) {
	content, toolCalls, toolResponse := splitParts(m.Parts)
	body := map[string]any{}
	switch m.Role {
	case "system":
		body["content"] = content
		return LogEvent{EventName: EventSystemMessage, Body: body}, true
	case "assistant":
		body["role"] = "assistant"
		if len(content) > 0 {
			body["content"] = content
		}
		if len(toolCalls) > 0 {
			body["tool_calls"] = toolCalls
		}
		return LogEvent{EventName: EventAssistantMessage, Body: body}, true
	case "tool":
		body["role"] = "tool"
		if toolResponse != nil {
			body["content"] = toolResponse
		} else if len(content) > 0 {
			body["content"] = content
		}
		return LogEvent{EventName: EventToolMessage, Body: body}, true
	default: // user (and any unknown role) render as a user message
		body["content"] = content
		return LogEvent{EventName: EventUserMessage, Body: body}, true
	}
}

// splitParts turns semconv parts into an OTel-event content array, a tool_calls
// array, and (for tool messages) a single tool response value.
func splitParts(parts []genai.Part) (content []map[string]any, toolCalls []map[string]any, toolResponse any) {
	for _, p := range parts {
		switch p.Type {
		case genai.PartText, genai.PartReasoning:
			content = append(content, map[string]any{"text": p.Content})
		case genai.PartToolCall:
			toolCalls = append(toolCalls, map[string]any{
				"id":   p.ID,
				"type": "function",
				"function": map[string]any{
					"name":      p.Name,
					"arguments": p.Arguments,
				},
			})
		case genai.PartToolCallResponse:
			toolResponse = p.Response
		}
	}
	return content, toolCalls, toolResponse
}

// joinText concatenates the text of a message's text/reasoning parts.
func joinText(parts []genai.Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == genai.PartText || p.Type == genai.PartReasoning {
			b.WriteString(p.Content)
		}
	}
	return b.String()
}

// textContentArray returns [{"text": ...}] for the text/reasoning parts.
func textContentArray(parts []genai.Part) []map[string]any {
	var out []map[string]any
	for _, p := range parts {
		if p.Type == genai.PartText || p.Type == genai.PartReasoning {
			out = append(out, map[string]any{"text": p.Content})
		}
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
