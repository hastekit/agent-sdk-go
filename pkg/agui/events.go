// Package agui implements the AG-UI Protocol
// (https://github.com/ag-ui-protocol/ag-ui) as a serving surface for
// agents. Agents publish responses.ResponseChunk events through the
// shared StreamBroker; this package translates each chunk into one or
// more AG-UI events and writes them out over SSE in the canonical wire
// format every AG-UI client (CopilotKit, raw @ag-ui/core, etc.)
// understands.
//
// Layout:
//   - events.go     : every AG-UI event struct (camelCase JSON tags,
//     stable type-tag strings)
//   - encoder.go    : SSE writer + framing
//   - translator.go : ResponseChunk → []Event state machine
//   - run_input.go  : the RunAgentInput request shape
//   - handler.go    : net/http handler exposing agents over AG-UI
//
// The translator is the only stateful piece. Everything else is
// declarative wire-shape code; treat the constants here as the
// authoritative contract with the protocol.
//
// Any Registry (e.g. *hastekit.SDK) can be exposed via NewHandler:
//
//	client, _ := hastekit.NewWithOptions(...)
//	client.NewAgent(...)
//	http.ListenAndServe(":8080", agui.NewHandler(client))
//
// For a ready-made browser chat UI on top of this protocol, see the
// pkg/agui/web subpackage.
package agui

import "encoding/json"

// EventType is the discriminator string AG-UI clients switch on.
// Values match the upstream protocol's SCREAMING_SNAKE_CASE
// EventType enum byte-for-byte — do not rename.
type EventType string

const (
	// Lifecycle
	EventRunStarted  EventType = "RUN_STARTED"
	EventRunFinished EventType = "RUN_FINISHED"
	EventRunError    EventType = "RUN_ERROR"

	// Step lifecycle (optional fine-grained run sub-stages). We emit
	// these around LLM turns so CopilotKit's "thinking..." indicators
	// have something to clamp onto.
	EventStepStarted  EventType = "STEP_STARTED"
	EventStepFinished EventType = "STEP_FINISHED"

	// Text messages
	EventTextMessageStart   EventType = "TEXT_MESSAGE_START"
	EventTextMessageContent EventType = "TEXT_MESSAGE_CONTENT"
	EventTextMessageEnd     EventType = "TEXT_MESSAGE_END"
	EventTextMessageChunk   EventType = "TEXT_MESSAGE_CHUNK"

	// Reasoning (private model reasoning surfaced to UI). The spec
	// has deprecated the THINKING_* names in favour of these; clients
	// at @ag-ui/client >= 0.0.50 subscribe with onReasoning*Event.
	EventReasoningStart          EventType = "REASONING_START"
	EventReasoningEnd            EventType = "REASONING_END"
	EventReasoningMessageStart   EventType = "REASONING_MESSAGE_START"
	EventReasoningMessageContent EventType = "REASONING_MESSAGE_CONTENT"
	EventReasoningMessageEnd     EventType = "REASONING_MESSAGE_END"
	EventReasoningMessageChunk   EventType = "REASONING_MESSAGE_CHUNK"
	EventReasoningEncryptedValue EventType = "REASONING_ENCRYPTED_VALUE"

	// Tool calls
	EventToolCallStart  EventType = "TOOL_CALL_START"
	EventToolCallArgs   EventType = "TOOL_CALL_ARGS"
	EventToolCallEnd    EventType = "TOOL_CALL_END"
	EventToolCallChunk  EventType = "TOOL_CALL_CHUNK"
	EventToolCallResult EventType = "TOOL_CALL_RESULT"

	// State
	EventStateSnapshot    EventType = "STATE_SNAPSHOT"
	EventStateDelta       EventType = "STATE_DELTA"
	EventMessagesSnapshot EventType = "MESSAGES_SNAPSHOT"

	// Escape hatches
	EventRaw    EventType = "RAW"
	EventCustom EventType = "CUSTOM"
)

// Role values AG-UI assigns to messages. Mirrors the protocol enum.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
	RoleDeveloper Role = "developer"
	// RoleReasoning marks a standalone reasoning message in a
	// MessagesSnapshot. The live stream surfaces reasoning via the
	// REASONING_MESSAGE_* events (also role "reasoning"); on history
	// reload it rehydrates as a message with this role instead.
	RoleReasoning Role = "reasoning"
)

// Event is the common interface every AG-UI event satisfies. The
// translator returns []Event so the encoder can iterate without
// caring about the concrete type. EventType() lets the encoder
// stamp the SSE `event:` field; Marshal returns the wire JSON body.
type Event interface {
	EventType() EventType
	Marshal() ([]byte, error)
}

// BaseEvent carries the fields every concrete event embeds.
// `timestamp` is unix millis to match the JS Date.now() the
// reference TypeScript implementation emits.
//
// `rawEvent` is the optional pass-through of the upstream chunk
// the event was derived from — useful for clients that want to
// fall back to the raw responses.ResponseChunk shape. We populate
// it sparingly (RAW events only) to keep wire size down.
type BaseEvent struct {
	Type      EventType `json:"type"`
	Timestamp int64     `json:"timestamp,omitempty"`
	RawEvent  any       `json:"rawEvent,omitempty"`
}

// ── Lifecycle ──────────────────────────────────────────────────────

// RunStartedEvent opens a run. threadId+runId together identify the
// conversation turn for the AG-UI client.
type RunStartedEvent struct {
	BaseEvent
	ThreadID string `json:"threadId"`
	RunID    string `json:"runId"`
}

func (e *RunStartedEvent) EventType() EventType { return EventRunStarted }
func (e *RunStartedEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// RunFinishedEvent closes a run. result is an opaque payload the
// agent may have produced (we set it to the run usage block).
type RunFinishedEvent struct {
	BaseEvent
	ThreadID string `json:"threadId"`
	RunID    string `json:"runId"`
	Result   any    `json:"result,omitempty"`
}

func (e *RunFinishedEvent) EventType() EventType { return EventRunFinished }
func (e *RunFinishedEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// RunErrorEvent terminates a run with an error. code is optional.
type RunErrorEvent struct {
	BaseEvent
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

func (e *RunErrorEvent) EventType() EventType { return EventRunError }
func (e *RunErrorEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// ── Steps ──────────────────────────────────────────────────────────

type StepStartedEvent struct {
	BaseEvent
	StepName string `json:"stepName"`
}

func (e *StepStartedEvent) EventType() EventType { return EventStepStarted }
func (e *StepStartedEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

type StepFinishedEvent struct {
	BaseEvent
	StepName string `json:"stepName"`
}

func (e *StepFinishedEvent) EventType() EventType { return EventStepFinished }
func (e *StepFinishedEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// ── Text messages ─────────────────────────────────────────────────

type TextMessageStartEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
	Role      Role   `json:"role"`
}

func (e *TextMessageStartEvent) EventType() EventType { return EventTextMessageStart }
func (e *TextMessageStartEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

type TextMessageContentEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
	Delta     string `json:"delta"`
}

func (e *TextMessageContentEvent) EventType() EventType { return EventTextMessageContent }
func (e *TextMessageContentEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

type TextMessageEndEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
}

func (e *TextMessageEndEvent) EventType() EventType { return EventTextMessageEnd }
func (e *TextMessageEndEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// ── Thinking / reasoning ──────────────────────────────────────────

// ReasoningStartEvent opens a reasoning block (the LLM's private
// chain-of-thought). Title is optional; some clients use it as a
// header above the collapsed reasoning panel. MessageID is required by
// the AG-UI schema and ties the block to its message/content/end
// events.
type ReasoningStartEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
	Title     string `json:"title,omitempty"`
}

func (e *ReasoningStartEvent) EventType() EventType { return EventReasoningStart }
func (e *ReasoningStartEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

type ReasoningEndEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
}

func (e *ReasoningEndEvent) EventType() EventType { return EventReasoningEnd }
func (e *ReasoningEndEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// ReasoningMessageStartEvent opens the reasoning message. The AG-UI
// schema requires both messageId and role (which must be "reasoning").
type ReasoningMessageStartEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
}

func (e *ReasoningMessageStartEvent) EventType() EventType { return EventReasoningMessageStart }
func (e *ReasoningMessageStartEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

type ReasoningMessageContentEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
	Delta     string `json:"delta"`
}

func (e *ReasoningMessageContentEvent) EventType() EventType {
	return EventReasoningMessageContent
}
func (e *ReasoningMessageContentEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

type ReasoningMessageEndEvent struct {
	BaseEvent
	MessageID string `json:"messageId"`
}

func (e *ReasoningMessageEndEvent) EventType() EventType { return EventReasoningMessageEnd }
func (e *ReasoningMessageEndEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// ReasoningEncryptedValueEvent carries provider-encrypted reasoning
// blobs (Anthropic's `thought_signature`, etc.) so the client can
// echo them back on the next turn for stateful reasoning models.
type ReasoningEncryptedValueEvent struct {
	BaseEvent
	Value string `json:"value"`
}

func (e *ReasoningEncryptedValueEvent) EventType() EventType { return EventReasoningEncryptedValue }
func (e *ReasoningEncryptedValueEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// ── Tool calls ────────────────────────────────────────────────────

type ToolCallStartEvent struct {
	BaseEvent
	ToolCallID      string `json:"toolCallId"`
	ToolCallName    string `json:"toolCallName"`
	ParentMessageID string `json:"parentMessageId,omitempty"`
}

func (e *ToolCallStartEvent) EventType() EventType { return EventToolCallStart }
func (e *ToolCallStartEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

type ToolCallArgsEvent struct {
	BaseEvent
	ToolCallID string `json:"toolCallId"`
	Delta      string `json:"delta"`
}

func (e *ToolCallArgsEvent) EventType() EventType { return EventToolCallArgs }
func (e *ToolCallArgsEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

type ToolCallEndEvent struct {
	BaseEvent
	ToolCallID string `json:"toolCallId"`
}

func (e *ToolCallEndEvent) EventType() EventType { return EventToolCallEnd }
func (e *ToolCallEndEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// ToolCallResultEvent reports the tool's output back to the client.
// Role is always "tool" per the spec; messageId identifies the
// synthetic tool-message that holds the result in the conversation
// log (mirrors OpenAI's tool-result message shape).
type ToolCallResultEvent struct {
	BaseEvent
	MessageID  string `json:"messageId"`
	ToolCallID string `json:"toolCallId"`
	Content    string `json:"content"`
	Role       Role   `json:"role"`
}

func (e *ToolCallResultEvent) EventType() EventType { return EventToolCallResult }
func (e *ToolCallResultEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	if e.Role == "" {
		e.Role = RoleTool
	}
	return json.Marshal(e)
}

// ── State ─────────────────────────────────────────────────────────

// StateSnapshotEvent ships the complete agent state object. Clients
// that use CopilotKit's useCoAgent() read this as the latest
// authoritative state.
type StateSnapshotEvent struct {
	BaseEvent
	Snapshot any `json:"snapshot"`
}

func (e *StateSnapshotEvent) EventType() EventType { return EventStateSnapshot }
func (e *StateSnapshotEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// StateDeltaEvent applies an RFC 6902 JSON Patch to the client's
// current state. The delta is opaque to us — the wire shape is ready
// even though nothing in the SDK publishes state deltas today.
type StateDeltaEvent struct {
	BaseEvent
	Delta []map[string]any `json:"delta"`
}

func (e *StateDeltaEvent) EventType() EventType { return EventStateDelta }
func (e *StateDeltaEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// MessagesSnapshotEvent is the canonical AG-UI messages list. Each
// entry has the spec's role+content+toolCalls shape.
type MessagesSnapshotEvent struct {
	BaseEvent
	Messages []Message `json:"messages"`
}

func (e *MessagesSnapshotEvent) EventType() EventType { return EventMessagesSnapshot }
func (e *MessagesSnapshotEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// Message is the AG-UI message shape (used inside MessagesSnapshot
// and RunAgentInput). Strict superset of the OpenAI chat message:
// role/content/name plus optional toolCalls array on assistant
// messages, plus toolCallId on tool messages.
type Message struct {
	ID         string     `json:"id"`
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	// EncryptedValue carries provider-encrypted reasoning on a
	// role="reasoning" message, per the AG-UI reasoning message shape.
	// Empty (and omitted) on every other role.
	EncryptedValue string `json:"encryptedValue,omitempty"`
}

// ToolCall is the function-call shape embedded inside an assistant
// Message. Matches OpenAI's function-call schema; CopilotKit reads
// it as-is.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ── Escape hatches ────────────────────────────────────────────────

// RawEvent ships an upstream event verbatim. We use this for chunk
// shapes that don't map cleanly to any AG-UI event but might still
// be interesting to a power-user client.
type RawEvent struct {
	BaseEvent
	Event  any    `json:"event"`
	Source string `json:"source,omitempty"`
}

func (e *RawEvent) EventType() EventType { return EventRaw }
func (e *RawEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// CustomEvent is the protocol's named-extension hatch. We use it for
// hastekit-specific signals (approval prompts, generated files) under
// the "hastekit.*" namespace so AG-UI-strict clients can ignore them
// and CopilotKit-with-hastekit-extensions can pick them up.
type CustomEvent struct {
	BaseEvent
	Name  string `json:"name"`
	Value any    `json:"value"`
}

func (e *CustomEvent) EventType() EventType { return EventCustom }
func (e *CustomEvent) Marshal() ([]byte, error) {
	e.BaseEvent.Type = e.EventType()
	return json.Marshal(e)
}

// Custom event names. AG-UI 0.0.53 has no native INTERRUPT event
// type; the de-facto standard (used by CopilotKit's useInterrupt
// hook and LangGraph's interrupt protocol) is a CUSTOM event named
// "on_interrupt" carrying an application-defined value payload,
// with the run terminated via RUN_FINISHED immediately after so the
// onRunFinalized hook fires. The resume contract uses
// forwardedProps.command.resume on the next POST.
//
// Everything else stays under the "hastekit.*" namespace so AG-UI-
// strict clients can ignore them without breaking on unknown
// custom events.
const (
	CustomNameInterrupt     = "on_interrupt"
	CustomNameFileGenerated = "hastekit.file_generated"
	CustomNameAnnotation    = "hastekit.annotation"
	CustomNameStreamID      = "hastekit.stream_id"
	CustomNameToolProgress  = "hastekit.tool_progress"

	// Background tasks: a tool answered its call and kept working. Started
	// carries where to follow it; completed says the agent has taken the
	// result in.
	CustomNameBackgroundTaskStarted   = "hastekit.background_task_started"
	CustomNameBackgroundTaskCompleted = "hastekit.background_task_completed"
)
