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
	require.Equal(t, []EventType{EventRunFinished}, types, "the pause is the outcome, not a custom event")

	outcome := events[0].(*RunFinishedEvent).Outcome
	require.NotNil(t, outcome)
	assert.Equal(t, OutcomeInterrupt, outcome.Type)
	require.Len(t, outcome.Interrupts, 1)
	assert.Equal(t, "call_1", outcome.Interrupts[0].ToolCallID)
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

	// Replayed/internal completed chunks may already carry the durable
	// reference; expose the same authorized route history uses.
	refEvents := NewTranslator("thread-1", "run-2").Translate(imageDone(
		"ig_2", "png", "attachment://0123456789abcdef0123456789abcdef",
	))
	require.Len(t, refEvents, 3)
	assert.Equal(t,
		"![generated image](/api/agui/attachments/0123456789abcdef0123456789abcdef)",
		refEvents[1].(*TextMessageContentEvent).Delta,
	)
}

func TestCompletedResponseRendersMissingImages(t *testing.T) {
	for _, doneResult := range []string{"", "BBBB"} {
		t.Run("done result="+doneResult, func(t *testing.T) {
			tr := NewTranslator("thread-1", "run-1")
			tr.Start()
			tr.Translate(&responses.ResponseChunk{
				OfResponseCreated: &responses.ChunkResponse[constants.ChunkTypeResponseCreated]{},
			})
			// An empty done result must not prevent the final image from rendering.
			doneEvents := tr.Translate(imageDone("ig_1", "png", doneResult))
			completed := &responses.ResponseChunk{
				OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{
					Response: responses.ChunkResponseData{Output: []responses.OutputMessageUnion{
						{OfImageGenerationCall: &responses.ImageGenerationCallMessage{ID: "ig_1", OutputFormat: "png", Result: "BBBB"}},
						{OfImageGenerationCall: &responses.ImageGenerationCallMessage{ID: "ig_2", OutputFormat: "png", Result: "CCCC"}},
					}},
				},
			}
			completedEvents := tr.Translate(completed)
			require.Equal(t, EventStepFinished, completedEvents[len(completedEvents)-1].EventType())
			var images []*TextMessageContentEvent
			for _, event := range append(doneEvents, completedEvents...) {
				if content, ok := event.(*TextMessageContentEvent); ok {
					images = append(images, content)
				}
			}
			require.Len(t, images, 2)
			assert.Equal(t, "ig_1", images[0].MessageID)
			assert.Equal(t, "![generated image](data:image/png;base64,BBBB)", images[0].Delta)
			assert.Equal(t, "ig_2", images[1].MessageID)
			assert.Equal(t, "![generated image](data:image/png;base64,CCCC)", images[1].Delta)
			assert.Empty(t, tr.Translate(completed), "repeated completed events must not duplicate images")
			assert.Empty(t, tr.Translate(imageDone("ig_2", "png", "CCCC")))
		})
	}
}

func runPausedWith(interrupts ...responses.Interrupt) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfRunPaused: &responses.ChunkRun[constants.ChunkTypeRunPaused]{
			RunState: responses.ChunkRunData{PendingInterrupts: interrupts},
		},
	}
}

// pauseOutcome translates a pause and returns its RUN_FINISHED outcome.
func pauseOutcome(t *testing.T, chunk *responses.ResponseChunk) *RunFinishedOutcome {
	t.Helper()
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()
	events := tr.Translate(chunk)
	require.Equal(t, []EventType{EventRunFinished}, eventTypes(events), "a pause neither replaces the client's state nor emits a custom event")
	finished := events[0].(*RunFinishedEvent)
	require.NotNil(t, finished.Outcome)
	return finished.Outcome
}

// A form elicitation reaches the client as an input_required interrupt with the
// schema it has to render.
func TestRunPausedEmitsFormElicitation(t *testing.T) {
	outcome := pauseOutcome(t, runPausedWith(responses.Interrupt{
		FunctionCallMessage: responses.FunctionCallMessage{
			CallID: "call_1", Name: "book_flight", Arguments: `{"flight_no":"TP1234"}`,
		},
		Mode: responses.InterruptModeForm,
		Elicitations: []mcp.ElicitParams{{
			Message:         "Passenger details, as printed on the passport.",
			RequestedSchema: map[string]any{"type": "object"},
		}},
	}))

	assert.Equal(t, OutcomeInterrupt, outcome.Type)
	require.Len(t, outcome.Interrupts, 1)
	it := outcome.Interrupts[0]
	assert.Equal(t, "call_1", it.ID)
	assert.Equal(t, "call_1", it.ToolCallID)
	assert.Equal(t, InterruptReasonInputRequired, it.Reason)
	assert.Equal(t, "Passenger details, as printed on the passport.", it.Message)
	assert.Equal(t, map[string]any{"type": "object"}, it.ResponseSchema)
	assert.Equal(t, "form", it.Metadata["mode"])
	assert.Equal(t, "book_flight", it.Metadata["toolName"])
}

func TestRunPausedEmitsURLElicitation(t *testing.T) {
	outcome := pauseOutcome(t, runPausedWith(responses.Interrupt{
		FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_1", Name: "link_loyalty"},
		Mode:                responses.InterruptModeURL,
		Elicitations: []mcp.ElicitParams{{
			Message: "Connect your loyalty account to continue.",
			URL:     "https://example.test/oauth/start",
		}},
	}))

	require.Len(t, outcome.Interrupts, 1)
	assert.Equal(t, InterruptReasonInputRequired, outcome.Interrupts[0].Reason)
	assert.Equal(t, "url", outcome.Interrupts[0].Metadata["mode"])
	assert.Equal(t, "https://example.test/oauth/start", outcome.Interrupts[0].Metadata["url"])
}

// An approval is a tool_call interrupt answered with {"approved": bool}.
func TestApprovalPauseIsAToolCallInterrupt(t *testing.T) {
	outcome := pauseOutcome(t, runPaused(responses.FunctionCallMessage{
		CallID: "call_1", Name: "issue_refund", Arguments: "{}",
	}))

	require.Len(t, outcome.Interrupts, 1)
	it := outcome.Interrupts[0]
	assert.Equal(t, InterruptReasonToolCall, it.Reason)
	assert.Equal(t, "call_1", it.ToolCallID)
	assert.Equal(t, approvalSchema, it.ResponseSchema)
}

// A run can pause on both at once; the outcome lists each with its own reason.
func TestMixedPauseSeparatesApprovalsFromElicitations(t *testing.T) {
	outcome := pauseOutcome(t, runPausedWith(
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

	require.Len(t, outcome.Interrupts, 2)
	assert.Equal(t, InterruptReasonToolCall, outcome.Interrupts[0].Reason)
	assert.Equal(t, InterruptReasonInputRequired, outcome.Interrupts[1].Reason)
}

// A pause only on client tools is a successful run that leaves the calls
// unanswered for the client; there is nothing for a person to answer, so no
// interrupt event. The outcome does not name the calls: pre-1.0 AG-UI clients
// reject pendingToolCallIds.
func TestClientToolPauseIsASuccessWithUnansweredCalls(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()
	events := tr.Translate(runPausedWith(
		responses.Interrupt{FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_a", Name: "get_selection"}, Mode: responses.InterruptModeClientTool},
		responses.Interrupt{FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_b", Name: "get_time"}, Mode: responses.InterruptModeClientTool},
	))
	require.Equal(t, []EventType{EventRunFinished}, eventTypes(events))
	outcome := events[0].(*RunFinishedEvent).Outcome
	assert.Equal(t, &RunFinishedOutcome{Type: OutcomeSuccess}, outcome)

	// Mixed with an approval, the interrupt wins and the client tool call stays unanswered in the stream.
	outcome = pauseOutcome(t, runPausedWith(
		responses.Interrupt{FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_a", Name: "get_selection"}, Mode: responses.InterruptModeClientTool},
		responses.Interrupt{FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_1", Name: "issue_refund"}, Mode: responses.InterruptModeApproval},
	))
	assert.Equal(t, OutcomeInterrupt, outcome.Type)
	require.Len(t, outcome.Interrupts, 1)
	assert.Equal(t, "call_1", outcome.Interrupts[0].ID)
}

// An interrupt with no mode set is an approval — the synthesized shape older
// state rows and tool-level RequiresApproval gates produce.
func TestUnsetModeProjectsAsApproval(t *testing.T) {
	outcome := pauseOutcome(t, runPausedWith(responses.Interrupt{
		FunctionCallMessage: responses.FunctionCallMessage{CallID: "call_1", Name: "t"},
	}))
	require.Len(t, outcome.Interrupts, 1)
	assert.Equal(t, InterruptReasonToolCall, outcome.Interrupts[0].Reason)
	assert.Equal(t, "approval", outcome.Interrupts[0].Metadata["mode"])
}

func inputMessageChunk(id, role, content string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfInputMessage: &responses.ChunkInputMessage[constants.ChunkTypeInputMessage]{
			MessageID: id, Role: role, Content: content,
		},
	}
}

// A turn the run took in becomes an ordinary AG-UI text message under the
// author's role, so a client needs nothing new to place it.
func TestInputMessageBecomesATextMessage(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	events := tr.Translate(inputMessageChunk("msg_u1", "user", "hurry up"))
	require.Equal(t, []EventType{
		EventTextMessageStart, EventTextMessageContent, EventTextMessageEnd,
	}, eventTypes(events))

	start := events[0].(*TextMessageStartEvent)
	assert.Equal(t, "msg_u1", start.MessageID)
	assert.Equal(t, RoleUser, start.Role, "the user's turn is not attributed to the agent")
	assert.Equal(t, "hurry up", events[1].(*TextMessageContentEvent).Delta)
}

// It closes nothing and opens nothing: an assistant message that happened to
// be mid-flight has to still be mid-flight afterwards, or its remaining deltas
// arrive against a message the client has already closed.
func TestInputMessageLeavesAnOpenAssistantMessageAlone(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()
	tr.Translate(messageAdded("msg_a1"))

	tr.Translate(inputMessageChunk("msg_u1", "user", "hurry up"))

	// No re-open: the assistant message the translator was already holding is
	// still the one a delta belongs to.
	assert.Equal(t, []EventType{EventTextMessageContent},
		eventTypes(tr.Translate(textDelta("msg_a1", "still going"))))
}

// The grounding context the handler appends is written for the model, and is
// stripped on rehydration for the same reason it is stripped here.
func TestInputMessageStripsTheContextBlock(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	events := tr.Translate(inputMessageChunk("msg_u1", "user",
		"what is the weather?\n\n<context>\nlocation: Paris\n</context>"))
	require.Len(t, events, 3)
	assert.Equal(t, "what is the weather?", events[1].(*TextMessageContentEvent).Delta)
}

// A turn with nothing left after stripping says nothing at all, rather than
// opening an empty message the client then has to render.
func TestInputMessageWithNoTextIsDropped(t *testing.T) {
	tr := NewTranslator("thread-1", "run-1")
	tr.Start()

	assert.Empty(t, tr.Translate(inputMessageChunk("msg_u1", "user",
		"<context>\nlocation: Paris\n</context>")))
}
