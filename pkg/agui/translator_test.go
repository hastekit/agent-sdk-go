package agui

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func messageAdded(itemID string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Item: responses.ChunkOutputItemData{Type: "message", Id: itemID},
		},
	}
}

func messageDone(itemID string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Item: responses.ChunkOutputItemData{Type: "message", Id: itemID},
		},
	}
}

func textDelta(itemID, delta string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{
			ItemId: itemID,
			Delta:  delta,
		},
	}
}

func functionCallAdded(itemID, callID, name string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Item: responses.ChunkOutputItemData{
				Type:   "function_call",
				Id:     itemID,
				CallID: utils.Ptr(callID),
				Name:   utils.Ptr(name),
			},
		},
	}
}

func argsDelta(itemID, delta string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfFunctionCallArgumentsDelta: &responses.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]{
			ItemId: itemID,
			Delta:  delta,
		},
	}
}

func runCompleted() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfRunCompleted: &responses.ChunkRun[constants.ChunkTypeRunCompleted]{},
	}
}

func runPaused(calls ...responses.FunctionCallMessage) *responses.ResponseChunk {
	interrupts := make([]responses.Interrupt, 0, len(calls))
	for _, c := range calls {
		interrupts = append(interrupts, responses.Interrupt{
			FunctionCallMessage: c,
			Mode:                responses.InterruptModeApproval,
		})
	}
	return &responses.ResponseChunk{
		OfRunPaused: &responses.ChunkRun[constants.ChunkTypeRunPaused]{
			RunState: responses.ChunkRunData{PendingInterrupts: interrupts},
		},
	}
}

func reasoningAdded(itemID string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Item: responses.ChunkOutputItemData{Type: "reasoning", Id: itemID},
		},
	}
}

func reasoningDone(itemID string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Item: responses.ChunkOutputItemData{Type: "reasoning", Id: itemID},
		},
	}
}

func reasoningDelta(itemID, delta string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningTextDelta: &responses.ChunkReasoningText[constants.ChunkTypeReasoningTextDelta]{
			ItemId: itemID,
			Delta:  delta,
		},
	}
}

func eventTypes(events []Event) []EventType {
	out := make([]EventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.EventType())
	}
	return out
}

func TestTextMessageBracketing(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")

	assert.Equal(t, []EventType{EventRunStarted}, eventTypes(tr.Start()))
	assert.Equal(t, []EventType{EventTextMessageStart}, eventTypes(tr.Translate(messageAdded("msg_1"))))
	assert.Equal(t, []EventType{EventTextMessageContent}, eventTypes(tr.Translate(textDelta("msg_1", "Hello"))))
	assert.Equal(t, []EventType{EventTextMessageEnd}, eventTypes(tr.Translate(messageDone("msg_1"))))
	assert.Equal(t, []EventType{EventRunFinished}, eventTypes(tr.Translate(runCompleted())))
}

func TestLazyTextMessageOpenOnDelta(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	// Delta without a preceding item_added still produces a valid
	// START → CONTENT sequence.
	events := tr.Translate(textDelta("msg_1", "Hi"))
	assert.Equal(t, []EventType{EventTextMessageStart, EventTextMessageContent}, eventTypes(events))
}

func TestReasoningEventsCarryMessageID(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	events := tr.Translate(reasoningDelta("reason_1", "thinking"))
	require.Equal(t, []EventType{
		EventReasoningStart, EventReasoningMessageStart, EventReasoningMessageContent,
	}, eventTypes(events))

	// Every reasoning event must carry the item id as messageId (the
	// AG-UI schema requires it), and MESSAGE_START must carry role
	// "reasoning".
	assert.Equal(t, "reason_1", events[0].(*ReasoningStartEvent).MessageID)
	start := events[1].(*ReasoningMessageStartEvent)
	assert.Equal(t, "reason_1", start.MessageID)
	assert.Equal(t, "reasoning", start.Role)
	content := events[2].(*ReasoningMessageContentEvent)
	assert.Equal(t, "reason_1", content.MessageID)
	assert.Equal(t, "thinking", content.Delta)

	end := tr.Translate(reasoningDone("reason_1"))
	require.Equal(t, []EventType{EventReasoningMessageEnd, EventReasoningEnd}, eventTypes(end))
	assert.Equal(t, "reason_1", end[0].(*ReasoningMessageEndEvent).MessageID)
	assert.Equal(t, "reason_1", end[1].(*ReasoningEndEvent).MessageID)
}

// An empty reasoning item — added then done with no content delta in
// between — is exactly what tripped the CopilotKit Zod validator when
// the END events lacked a messageId. It must still produce a fully
// bracketed, messageId-carrying sequence.
func TestEmptyReasoningItemIsWellFormed(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	open := tr.Translate(reasoningAdded("reason_1"))
	require.Equal(t, []EventType{EventReasoningStart, EventReasoningMessageStart}, eventTypes(open))

	end := tr.Translate(reasoningDone("reason_1"))
	require.Equal(t, []EventType{EventReasoningMessageEnd, EventReasoningEnd}, eventTypes(end))

	for _, e := range append(open, end...) {
		switch ev := e.(type) {
		case *ReasoningStartEvent:
			assert.Equal(t, "reason_1", ev.MessageID)
		case *ReasoningMessageStartEvent:
			assert.Equal(t, "reason_1", ev.MessageID)
			assert.Equal(t, "reasoning", ev.Role)
		case *ReasoningMessageEndEvent:
			assert.Equal(t, "reason_1", ev.MessageID)
		case *ReasoningEndEvent:
			assert.Equal(t, "reason_1", ev.MessageID)
		}
	}
}

func TestToolCallArgsResolveItemIDToCallID(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	events := tr.Translate(functionCallAdded("item_1", "call_1", "get_weather"))
	require.Equal(t, []EventType{EventToolCallStart}, eventTypes(events))
	start := events[0].(*ToolCallStartEvent)
	assert.Equal(t, "call_1", start.ToolCallID)
	assert.Equal(t, "get_weather", start.ToolCallName)

	events = tr.Translate(argsDelta("item_1", `{"city":`))
	require.Equal(t, []EventType{EventToolCallArgs}, eventTypes(events))
	assert.Equal(t, "call_1", events[0].(*ToolCallArgsEvent).ToolCallID)

	// Unknown item ids are dropped, not crashed on.
	assert.Empty(t, tr.Translate(argsDelta("item_unknown", "x")))
}

func TestRunCompletedClosesOpenItems(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()
	tr.Translate(messageAdded("msg_1"))
	tr.Translate(functionCallAdded("item_1", "call_1", "tool"))
	tr.Translate(&responses.ResponseChunk{
		OfResponseCreated: &responses.ChunkResponse[constants.ChunkTypeResponseCreated]{},
	})

	types := eventTypes(tr.Translate(runCompleted()))
	// Open text message, tool call, and step all get closed before
	// the terminal RUN_FINISHED — and RUN_FINISHED is last.
	assert.Contains(t, types, EventTextMessageEnd)
	assert.Contains(t, types, EventToolCallEnd)
	assert.Contains(t, types, EventStepFinished)
	assert.Equal(t, EventRunFinished, types[len(types)-1])
}

func TestRunPausedEmitsInterruptThenFinished(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	events := tr.Translate(runPaused(responses.FunctionCallMessage{
		CallID: "call_1", Name: "dangerous_tool", Arguments: "{}",
	}))
	types := eventTypes(events)
	require.Equal(t, []EventType{EventStateSnapshot, EventCustom, EventRunFinished}, types)

	custom := events[1].(*CustomEvent)
	assert.Equal(t, CustomNameInterrupt, custom.Name)
	value := custom.Value.(map[string]any)
	assert.Equal(t, "tool_approval", value["kind"])
	pending := value["pendingToolCalls"].([]map[string]any)
	require.Len(t, pending, 1)
	assert.Equal(t, "call_1", pending[0]["toolCallId"])
}

func TestStepsDedupeAndPair(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	created := &responses.ResponseChunk{
		OfResponseCreated: &responses.ChunkResponse[constants.ChunkTypeResponseCreated]{},
	}
	completed := &responses.ResponseChunk{
		OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{},
	}

	assert.Equal(t, []EventType{EventStepStarted}, eventTypes(tr.Translate(created)))
	// Duplicate start with the same name is swallowed.
	assert.Empty(t, tr.Translate(created))
	assert.Equal(t, []EventType{EventStepFinished}, eventTypes(tr.Translate(completed)))
	// Unmatched finish is swallowed too.
	assert.Empty(t, tr.Translate(completed))
}

func TestFinishSynthesisesRunFinished(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()
	tr.Translate(messageAdded("msg_1"))

	types := eventTypes(tr.Finish())
	assert.Equal(t, []EventType{EventTextMessageEnd, EventRunFinished}, types)
}

func imageDone(itemID, format, b64 string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Item: responses.ChunkOutputItemData{
				Type:         "image_generation_call",
				Id:           itemID,
				OutputFormat: utils.Ptr(format),
				Result:       utils.Ptr(b64),
			},
		},
	}
}

func TestImageGenerationEmitsOneMarkdownMessage(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	// Partial frames produce nothing (no duplicate images).
	partial := &responses.ResponseChunk{
		OfImageGenerationCallPartialImage: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{
			ItemId: "ig_1", PartialImageBase64: "AAAA",
		},
	}
	assert.Empty(t, tr.Translate(partial))

	// The completed image becomes a single assistant text message
	// carrying a markdown data-url image — no CUSTOM event.
	events := tr.Translate(imageDone("ig_1", "png", "BBBB"))
	assert.Equal(t, []EventType{EventTextMessageStart, EventTextMessageContent, EventTextMessageEnd}, eventTypes(events))
	content := events[1].(*TextMessageContentEvent)
	assert.Equal(t, "ig_1", content.MessageID)
	assert.Equal(t, "![generated image](data:image/png;base64,BBBB)", content.Delta)
	for _, e := range events {
		assert.NotEqual(t, EventCustom, e.EventType())
	}
}

func runPausedWith(interrupts ...responses.Interrupt) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfRunPaused: &responses.ChunkRun[constants.ChunkTypeRunPaused]{
			RunState: responses.ChunkRunData{PendingInterrupts: interrupts},
		},
	}
}

func pauseValue(t *testing.T, chunk *responses.ResponseChunk) (map[string]any, map[string]any) {
	t.Helper()
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()
	events := tr.Translate(chunk)
	require.Equal(t, []EventType{EventStateSnapshot, EventCustom, EventRunFinished}, eventTypes(events))
	return events[0].(*StateSnapshotEvent).Snapshot.(map[string]any),
		events[1].(*CustomEvent).Value.(map[string]any)
}

// A form elicitation must reach the client with the schema it has to render.
// Before this, non-approval interrupts were filtered out entirely: the run
// paused, the client was told nothing about what was wanted, and the run could
// never be resumed from a browser.
func TestRunPausedEmitsFormElicitation(t *testing.T) {
	snapshot, value := pauseValue(t, runPausedWith(responses.Interrupt{
		FunctionCallMessage: responses.FunctionCallMessage{
			CallID: "call_1", Name: "book_flight", Arguments: `{"flight_no":"TP1234"}`,
		},
		Mode: responses.InterruptModeForm,
		Elicitations: []mcp.ElicitParams{{
			Message:         "Passenger details, as printed on the passport.",
			RequestedSchema: map[string]any{"type": "object"},
		}},
	}))

	// No approve/reject verdict is outstanding, so an approval prompt must not
	// be raised for this pause.
	assert.Equal(t, false, snapshot["awaitingApproval"])
	assert.Empty(t, snapshot["pendingToolCalls"])

	assert.Equal(t, "elicitation", value["kind"])
	interrupts := value["interrupts"].([]map[string]any)
	require.Len(t, interrupts, 1)
	assert.Equal(t, "call_1", interrupts[0]["toolCallId"])
	assert.Equal(t, "book_flight", interrupts[0]["toolCallName"])
	assert.Equal(t, "form", interrupts[0]["mode"])
	assert.Equal(t, "Passenger details, as printed on the passport.", interrupts[0]["message"])
	assert.NotNil(t, interrupts[0]["requestedSchema"])
}

func TestRunPausedEmitsURLElicitation(t *testing.T) {
	_, value := pauseValue(t, runPausedWith(responses.Interrupt{
		FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_1", Name: "link_loyalty"},
		Mode:                responses.InterruptModeURL,
		Elicitations: []mcp.ElicitParams{{
			Message: "Connect your loyalty account to continue.",
			URL:     "https://example.test/oauth/start",
		}},
	}))

	interrupts := value["interrupts"].([]map[string]any)
	require.Len(t, interrupts, 1)
	assert.Equal(t, "url", interrupts[0]["mode"])
	assert.Equal(t, "https://example.test/oauth/start", interrupts[0]["url"])
}

// Clients written against the approval-only shape must keep working: an
// all-approval pause reports the same kind, flag and pendingToolCalls it
// always did.
func TestApprovalOnlyPauseKeepsLegacyShape(t *testing.T) {
	snapshot, value := pauseValue(t, runPaused(responses.FunctionCallMessage{
		CallID: "call_1", Name: "issue_refund", Arguments: "{}",
	}))

	assert.Equal(t, true, snapshot["awaitingApproval"])
	assert.Len(t, snapshot["pendingToolCalls"], 1)
	assert.Equal(t, "tool_approval", value["kind"])
	assert.Len(t, value["pendingToolCalls"], 1)
	assert.Len(t, value["interrupts"], 1)
}

// A run can pause on both at once. pendingToolCalls stays the approval subset
// so a legacy client renders exactly the decisions it can actually take.
func TestMixedPauseSeparatesApprovalsFromElicitations(t *testing.T) {
	snapshot, value := pauseValue(t, runPausedWith(
		responses.Interrupt{
			FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_1", Name: "issue_refund"},
			Mode:                responses.InterruptModeApproval,
		},
		responses.Interrupt{
			FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_2", Name: "book_flight"},
			Mode:                responses.InterruptModeForm,
			Elicitations:        []mcp.ElicitParams{{Message: "Passenger details"}},
		},
	))

	assert.Equal(t, true, snapshot["awaitingApproval"])
	pending := value["pendingToolCalls"].([]map[string]any)
	require.Len(t, pending, 1)
	assert.Equal(t, "call_1", pending[0]["toolCallId"])

	assert.Equal(t, "mixed", value["kind"])
	assert.Len(t, value["interrupts"], 2)
}

// An interrupt with no mode set is an approval — the synthesized shape older
// state rows and tool-level RequiresApproval gates produce.
func TestUnsetModeProjectsAsApproval(t *testing.T) {
	_, value := pauseValue(t, runPausedWith(responses.Interrupt{
		FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_1", Name: "t"},
	}))
	assert.Equal(t, "tool_approval", value["kind"])
	assert.Equal(t, "approval", value["interrupts"].([]map[string]any)[0]["mode"])
}
