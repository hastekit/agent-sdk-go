package openaicompat

import (
	"time"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

type openItemKind int

const (
	openItemNone openItemKind = iota
	openItemReasoning
	openItemMessage
	openItemFunctionCall
)

// StreamConverter turns a chat completions SSE stream into the native
// Responses event stream.
//
// A chat completion stream is flat - one delta object per frame, with text,
// reasoning and tool-call fragments distinguished only by which field is set.
// The Responses stream is nested: every piece of output is an item that must
// be opened (output_item.added), fed deltas, and closed (output_item.done)
// before the next one opens. This type holds the state needed to bridge the
// two: which item is currently open, what has accumulated into it, and the
// running output/sequence indices.
//
// Call Convert for every frame, then Finish once when the stream ends -
// Finish closes whatever item is still open and emits response.completed,
// which is where usage lands (it arrives in a trailing frame, after the frame
// that carried finish_reason).
type StreamConverter struct {
	responseID string
	model      string
	created    int64

	sequenceNumber int
	outputIndex    int
	started        bool
	completed      bool

	openKind    openItemKind
	openItemID  string
	accumulated string

	// Tool call state. toolIndex is the provider's index for the call
	// currently open, which is how argument fragments are attributed.
	toolIndex         int
	toolCallID        string
	toolName          string
	nextImplicitIndex int

	usage   *Usage
	outputs []responses.OutputMessageUnion
}

func NewStreamConverter() *StreamConverter {
	return &StreamConverter{outputs: []responses.OutputMessageUnion{}}
}

func (c *StreamConverter) nextSeqNum() int {
	n := c.sequenceNumber
	c.sequenceNumber++
	return n
}

// Convert translates one chat completion chunk into zero or more native
// chunks.
func (c *StreamConverter) Convert(in *ChatResponseChunk) []*responses.ResponseChunk {
	if in == nil {
		return nil
	}

	if in.ID != "" {
		c.responseID = in.ID
	}
	if in.Model != "" {
		c.model = in.Model
	}
	if in.Created != 0 {
		c.created = in.Created
	}
	if in.Usage != nil {
		c.usage = in.Usage
	}

	var out []*responses.ResponseChunk

	if !c.started {
		c.started = true
		out = append(out, c.buildResponseCreated(), c.buildResponseInProgress())
	}

	if len(in.Choices) == 0 {
		return out
	}

	// Only the first choice maps onto the Responses shape; n > 1 has no
	// native representation.
	choice := in.Choices[0]

	if choice.Delta.ReasoningContent != "" {
		out = append(out, c.openReasoning()...)
		c.accumulated += choice.Delta.ReasoningContent
		out = append(out, c.buildReasoningSummaryTextDelta(choice.Delta.ReasoningContent))
	}

	if choice.Delta.Content != "" {
		out = append(out, c.openMessage()...)
		c.accumulated += choice.Delta.Content
		out = append(out, c.buildOutputTextDelta(choice.Delta.Content))
	}

	for _, call := range choice.Delta.ToolCalls {
		out = append(out, c.handleToolCallDelta(call)...)
	}

	if choice.FinishReason != "" {
		out = append(out, c.closeOpenItem()...)
	}

	return out
}

// Finish closes any still-open item and emits response.completed. It is safe
// to call more than once.
func (c *StreamConverter) Finish() []*responses.ResponseChunk {
	if c.completed {
		return nil
	}
	c.completed = true

	out := c.closeOpenItem()

	if !c.started {
		// Stream ended without a single frame - still emit the envelope so
		// the consumer sees a well-formed run.
		c.started = true
		out = append(out, c.buildResponseCreated(), c.buildResponseInProgress())
	}

	return append(out, c.buildResponseCompleted())
}

func (c *StreamConverter) handleToolCallDelta(call ToolCall) []*responses.ResponseChunk {
	// `index` is what ties argument fragments to their call. A provider that
	// omits it has to be read off the fragments themselves: one carrying an
	// id starts a call, one carrying only arguments continues the open one.
	index := c.toolIndex
	switch {
	case call.Index != nil:
		index = *call.Index
	case c.openKind != openItemFunctionCall:
		index = c.nextImplicitIndex
	case call.ID != "" && call.ID != c.toolCallID:
		index = c.nextImplicitIndex
	}

	var out []*responses.ResponseChunk

	if c.openKind != openItemFunctionCall || c.toolIndex != index {
		out = append(out, c.closeOpenItem()...)

		c.openKind = openItemFunctionCall
		c.openItemID = responses.NewOutputItemFunctionCallID()
		c.toolIndex = index
		c.toolCallID = call.ID
		c.toolName = call.Function.Name
		c.accumulated = ""
		c.nextImplicitIndex = index + 1

		out = append(out, c.buildOutputItemAddedFunctionCall())
	}

	// id and name usually ride the first fragment, but some providers send
	// them late; take whichever fragment carries them.
	if call.ID != "" {
		c.toolCallID = call.ID
	}
	if call.Function.Name != "" && c.toolName == "" {
		c.toolName = call.Function.Name
	}

	if call.Function.Arguments != "" {
		c.accumulated += call.Function.Arguments
		out = append(out, c.buildFunctionCallArgumentsDelta(call.Function.Arguments))
	}

	return out
}

func (c *StreamConverter) openReasoning() []*responses.ResponseChunk {
	if c.openKind == openItemReasoning {
		return nil
	}

	out := c.closeOpenItem()
	c.openKind = openItemReasoning
	c.openItemID = responses.NewOutputItemReasoningID()
	c.accumulated = ""

	return append(out,
		c.buildOutputItemAddedReasoning(),
		c.buildReasoningSummaryPartAdded(),
	)
}

func (c *StreamConverter) openMessage() []*responses.ResponseChunk {
	if c.openKind == openItemMessage {
		return nil
	}

	out := c.closeOpenItem()
	c.openKind = openItemMessage
	c.openItemID = responses.NewOutputItemMessageID()
	c.accumulated = ""

	return append(out,
		c.buildOutputItemAddedMessage(),
		c.buildContentPartAddedText(),
	)
}

// closeOpenItem emits the done events for the item in flight, records it as a
// completed output, and advances the output index.
func (c *StreamConverter) closeOpenItem() []*responses.ResponseChunk {
	if c.openKind == openItemNone {
		return nil
	}

	text := c.accumulated
	var out []*responses.ResponseChunk

	switch c.openKind {
	case openItemReasoning:
		c.outputs = append(c.outputs, responses.OutputMessageUnion{
			OfReasoning: &responses.ReasoningMessage{
				ID:      c.openItemID,
				Summary: []responses.SummaryTextContent{{Text: text}},
			},
		})

		out = []*responses.ResponseChunk{
			c.buildReasoningSummaryTextDone(text),
			c.buildReasoningSummaryPartDone(text),
			c.buildOutputItemDoneReasoning(text),
		}

	case openItemMessage:
		c.outputs = append(c.outputs, responses.OutputMessageUnion{
			OfOutputMessage: &responses.OutputMessage{
				ID:   c.openItemID,
				Role: constants.RoleAssistant,
				Content: &responses.OutputContent{
					{OfOutputText: &responses.OutputTextContent{
						Text:        text,
						Annotations: []responses.Annotation{},
					}},
				},
			},
		})

		out = []*responses.ResponseChunk{
			c.buildOutputTextDone(text),
			c.buildContentPartDoneText(text),
			c.buildOutputItemDoneMessage(text),
		}

	case openItemFunctionCall:
		args := text
		if args == "" {
			args = "{}"
		}

		c.outputs = append(c.outputs, responses.OutputMessageUnion{
			OfFunctionCall: &responses.FunctionCallMessage{
				ID:        c.openItemID,
				CallID:    c.toolCallID,
				Name:      c.toolName,
				Arguments: args,
			},
		})

		out = []*responses.ResponseChunk{
			c.buildFunctionCallArgumentsDone(args),
			c.buildOutputItemDoneFunctionCall(args),
		}
	}

	c.openKind = openItemNone
	c.accumulated = ""
	c.outputIndex++

	return out
}

// =============================================================================
// Chunk Builders
// =============================================================================

func (c *StreamConverter) createdAt() int {
	if c.created != 0 {
		return int(c.created)
	}

	return int(time.Now().Unix())
}

func (c *StreamConverter) buildResponseCreated() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfResponseCreated: &responses.ChunkResponse[constants.ChunkTypeResponseCreated]{
			SequenceNumber: c.nextSeqNum(),
			Response: responses.ChunkResponseData{
				Id:        c.responseID,
				Object:    "response",
				CreatedAt: c.createdAt(),
				Status:    "in_progress",
				Request:   responses.Request{Model: c.model},
			},
		},
	}
}

func (c *StreamConverter) buildResponseInProgress() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfResponseInProgress: &responses.ChunkResponse[constants.ChunkTypeResponseInProgress]{
			SequenceNumber: c.nextSeqNum(),
			Response: responses.ChunkResponseData{
				Id:        c.responseID,
				Object:    "response",
				CreatedAt: c.createdAt(),
				Status:    "in_progress",
			},
		},
	}
}

func (c *StreamConverter) buildResponseCompleted() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{
			SequenceNumber: c.nextSeqNum(),
			Response: responses.ChunkResponseData{
				Id:        c.responseID,
				Object:    "response",
				CreatedAt: c.createdAt(),
				Status:    "completed",
				Output:    c.outputs,
				Usage:     nativeUsageOrEmpty(c.usage),
				Request:   responses.Request{Model: c.model},
			},
		},
	}
}

func (c *StreamConverter) buildOutputItemAddedMessage() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:    "message",
				Id:      c.openItemID,
				Status:  "in_progress",
				Role:    constants.RoleAssistant,
				Content: &responses.ChunkOutputItemContent{},
			},
		},
	}
}

func (c *StreamConverter) buildContentPartAddedText() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfContentPartAdded: &responses.ChunkContentPart[constants.ChunkTypeContentPartAdded]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Part:           responses.ChunkOutputItemContentUnion{OfOutputText: &responses.OutputTextContent{}},
		},
	}
}

func (c *StreamConverter) buildOutputTextDelta(delta string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Delta:          delta,
		},
	}
}

func (c *StreamConverter) buildOutputTextDone(text string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputTextDone: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDone]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Text:           utils.Ptr(text),
		},
	}
}

func (c *StreamConverter) buildContentPartDoneText(text string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfContentPartDone: &responses.ChunkContentPart[constants.ChunkTypeContentPartDone]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Part:           responses.ChunkOutputItemContentUnion{OfOutputText: &responses.OutputTextContent{Text: text}},
		},
	}
}

func (c *StreamConverter) buildOutputItemDoneMessage(text string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:    "message",
				Id:      c.openItemID,
				Status:  "completed",
				Role:    constants.RoleAssistant,
				Content: &responses.ChunkOutputItemContent{{OfOutputText: &responses.OutputTextContent{Text: text}}},
			},
		},
	}
}

func (c *StreamConverter) buildOutputItemAddedReasoning() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:    "reasoning",
				Id:      c.openItemID,
				Status:  "in_progress",
				Summary: &[]responses.SummaryTextContent{},
			},
		},
	}
}

func (c *StreamConverter) buildReasoningSummaryPartAdded() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningSummaryPartAdded: &responses.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartAdded]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Part:           responses.SummaryTextContent{},
		},
	}
}

func (c *StreamConverter) buildReasoningSummaryTextDelta(delta string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningSummaryTextDelta: &responses.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Delta:          delta,
		},
	}
}

func (c *StreamConverter) buildReasoningSummaryTextDone(text string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningSummaryTextDone: &responses.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDone]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Text:           utils.Ptr(text),
		},
	}
}

func (c *StreamConverter) buildReasoningSummaryPartDone(text string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningSummaryPartDone: &responses.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartDone]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Part:           responses.SummaryTextContent{Text: text},
		},
	}
}

func (c *StreamConverter) buildOutputItemDoneReasoning(text string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:    "reasoning",
				Id:      c.openItemID,
				Status:  "completed",
				Summary: &[]responses.SummaryTextContent{{Text: text}},
			},
		},
	}
}

func (c *StreamConverter) buildOutputItemAddedFunctionCall() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:      "function_call",
				Id:        c.openItemID,
				Status:    "in_progress",
				CallID:    utils.Ptr(c.toolCallID),
				Name:      utils.Ptr(c.toolName),
				Arguments: utils.Ptr(""),
			},
		},
	}
}

func (c *StreamConverter) buildFunctionCallArgumentsDelta(delta string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfFunctionCallArgumentsDelta: &responses.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Delta:          delta,
		},
	}
}

func (c *StreamConverter) buildFunctionCallArgumentsDone(args string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfFunctionCallArgumentsDone: &responses.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDone]{
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.openItemID,
			OutputIndex:    c.outputIndex,
			Arguments:      args,
		},
	}
}

func (c *StreamConverter) buildOutputItemDoneFunctionCall(args string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:      "function_call",
				Id:        c.openItemID,
				Status:    "completed",
				CallID:    utils.Ptr(c.toolCallID),
				Name:      utils.Ptr(c.toolName),
				Arguments: utils.Ptr(args),
			},
		},
	}
}
