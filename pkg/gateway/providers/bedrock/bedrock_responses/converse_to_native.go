package bedrock_responses

import (
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	responses "github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

// ConverseStreamToNativeConverter converts Bedrock ConverseStream events to native ResponseChunks.
// It maintains state across events to track current output index and accumulated content.
type ConverseStreamToNativeConverter struct {
	model          string
	responseID     string
	sequenceNumber int
	outputIndex    int

	// State for current content block
	currentBlockType string // "text" or "toolUse"
	currentToolUseId string
	currentToolName  string
	currentOutputID  string
	blockStarted     bool // whether output_item.added / content_part.added have been emitted

	completedOutputs []responses.OutputMessageUnion
	accumulatedText  string
	accumulatedArgs  string
	accumulatedSig   string // accumulated reasoning signature
	stopReason       string // from messageStop event

	// Usage from metadata event (arrives after messageStop in Bedrock)
	usage *responses.Usage
}

// NewConverseStreamToNativeConverter creates a new converter.
func NewConverseStreamToNativeConverter(model string) *ConverseStreamToNativeConverter {
	return &ConverseStreamToNativeConverter{
		model:      model,
		responseID: "resp_" + uuid.NewString(),
	}
}

func (c *ConverseStreamToNativeConverter) nextSeqNum() int {
	n := c.sequenceNumber
	c.sequenceNumber++
	return n
}

// ConvertEvent converts a single ConverseStream event to zero or more native ResponseChunks.
func (c *ConverseStreamToNativeConverter) ConvertEvent(event *ConverseStreamEvent) []*responses.ResponseChunk {
	if event == nil {
		return nil
	}

	switch {
	case event.MessageStart != nil:
		return c.handleMessageStart(event.MessageStart)
	case event.ContentBlockStart != nil:
		return c.handleContentBlockStart(event.ContentBlockStart)
	case event.ContentBlockDelta != nil:
		return c.handleContentBlockDelta(event.ContentBlockDelta)
	case event.ContentBlockStop != nil:
		return c.handleContentBlockStop(event.ContentBlockStop)
	case event.MessageStop != nil:
		return c.handleMessageStop(event.MessageStop)
	case event.Metadata != nil:
		return c.handleMetadata(event.Metadata)
	}

	return nil
}

func (c *ConverseStreamToNativeConverter) handleMessageStart(msg *StreamMessageStart) []*responses.ResponseChunk {
	return []*responses.ResponseChunk{
		{
			OfResponseCreated: &responses.ChunkResponse[constants.ChunkTypeResponseCreated]{
				Type:           constants.ChunkTypeResponseCreated("response.created"),
				SequenceNumber: c.nextSeqNum(),
				Response: responses.ChunkResponseData{
					Id:        c.responseID,
					Object:    "response",
					CreatedAt: int(time.Now().Unix()),
					Status:    "in_progress",
					Request: responses.Request{
						Model: c.model,
					},
				},
			},
		},
		{
			OfResponseInProgress: &responses.ChunkResponse[constants.ChunkTypeResponseInProgress]{
				Type:           constants.ChunkTypeResponseInProgress("response.in_progress"),
				SequenceNumber: c.nextSeqNum(),
				Response: responses.ChunkResponseData{
					Id:     c.responseID,
					Object: "response",
					Status: "in_progress",
				},
			},
		},
	}
}

func (c *ConverseStreamToNativeConverter) handleContentBlockStart(block *StreamContentBlockStart) []*responses.ResponseChunk {
	c.accumulatedText = ""
	c.accumulatedArgs = ""
	c.accumulatedSig = ""
	c.blockStarted = false

	if block.Start != nil && block.Start.ToolUse != nil {
		c.currentBlockType = "toolUse"
		c.currentToolUseId = block.Start.ToolUse.ToolUseId
		c.currentToolName = block.Start.ToolUse.Name
		c.currentOutputID = responses.NewOutputItemFunctionCallID()
		c.blockStarted = true

		return []*responses.ResponseChunk{
			c.buildOutputItemAddedFunctionCall(),
		}
	}

	if block.Start != nil && block.Start.ReasoningContent != nil {
		c.currentBlockType = "reasoning"
		c.currentOutputID = responses.NewOutputItemReasoningID()
		c.blockStarted = true

		return []*responses.ResponseChunk{
			c.buildOutputItemAddedReasoning(),
			c.buildReasoningSummaryPartAdded(),
		}
	}

	// Text content block
	c.currentBlockType = "text"
	c.currentOutputID = responses.NewOutputItemMessageID()
	c.blockStarted = true

	return []*responses.ResponseChunk{
		c.buildOutputItemAddedMessage(),
		c.buildContentPartAddedText(),
	}
}

// ensureTextBlockStarted emits output_item.added and content_part.added if they
// haven't been emitted yet. This handles cases where contentBlockStart is missing
// or failed to parse.
func (c *ConverseStreamToNativeConverter) ensureTextBlockStarted() []*responses.ResponseChunk {
	if c.blockStarted {
		return nil
	}
	c.blockStarted = true
	c.currentBlockType = "text"
	if c.currentOutputID == "" {
		c.currentOutputID = responses.NewOutputItemMessageID()
	}

	return []*responses.ResponseChunk{
		c.buildOutputItemAddedMessage(),
		c.buildContentPartAddedText(),
	}
}

// ensureReasoningBlockStarted emits output_item.added and reasoning_summary_part.added if needed.
func (c *ConverseStreamToNativeConverter) ensureReasoningBlockStarted() []*responses.ResponseChunk {
	if c.blockStarted {
		return nil
	}
	c.blockStarted = true
	c.currentBlockType = "reasoning"
	if c.currentOutputID == "" {
		c.currentOutputID = responses.NewOutputItemReasoningID()
	}

	return []*responses.ResponseChunk{
		c.buildOutputItemAddedReasoning(),
		c.buildReasoningSummaryPartAdded(),
	}
}

// ensureToolUseBlockStarted emits output_item.added if it hasn't been emitted yet.
func (c *ConverseStreamToNativeConverter) ensureToolUseBlockStarted() []*responses.ResponseChunk {
	if c.blockStarted {
		return nil
	}
	c.blockStarted = true
	c.currentBlockType = "toolUse"
	if c.currentOutputID == "" {
		c.currentOutputID = responses.NewOutputItemFunctionCallID()
	}

	return []*responses.ResponseChunk{
		c.buildOutputItemAddedFunctionCall(),
	}
}

func (c *ConverseStreamToNativeConverter) handleContentBlockDelta(delta *StreamContentBlockDelta) []*responses.ResponseChunk {
	if delta.Delta == nil {
		return nil
	}

	if delta.Delta.Text != nil {
		var chunks []*responses.ResponseChunk
		chunks = append(chunks, c.ensureTextBlockStarted()...)

		c.accumulatedText += *delta.Delta.Text
		chunks = append(chunks, &responses.ResponseChunk{
			OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{
				Type:           constants.ChunkTypeOutputTextDelta("response.output_text.delta"),
				SequenceNumber: c.nextSeqNum(),
				ItemId:         c.currentOutputID,
				OutputIndex:    c.outputIndex,
				ContentIndex:   0,
				Delta:          *delta.Delta.Text,
			},
		})
		return chunks
	}

	if delta.Delta.ToolUse != nil {
		var chunks []*responses.ResponseChunk
		chunks = append(chunks, c.ensureToolUseBlockStarted()...)

		c.accumulatedArgs += delta.Delta.ToolUse.Input
		chunks = append(chunks, &responses.ResponseChunk{
			OfFunctionCallArgumentsDelta: &responses.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]{
				Type:           constants.ChunkTypeFunctionCallArgumentsDelta("response.function_call_arguments.delta"),
				SequenceNumber: c.nextSeqNum(),
				ItemId:         c.currentOutputID,
				OutputIndex:    c.outputIndex,
				Delta:          delta.Delta.ToolUse.Input,
			},
		})
		return chunks
	}

	if delta.Delta.ReasoningContent != nil {
		var chunks []*responses.ResponseChunk
		chunks = append(chunks, c.ensureReasoningBlockStarted()...)

		if delta.Delta.ReasoningContent.Text != "" {
			c.accumulatedText += delta.Delta.ReasoningContent.Text
			chunks = append(chunks, c.buildReasoningSummaryTextDelta(delta.Delta.ReasoningContent.Text))
		}
		if delta.Delta.ReasoningContent.Signature != "" {
			c.accumulatedSig += delta.Delta.ReasoningContent.Signature
			chunks = append(chunks, c.buildReasoningSignatureDelta(delta.Delta.ReasoningContent.Signature))
		}
		return chunks
	}

	return nil
}

func (c *ConverseStreamToNativeConverter) handleContentBlockStop(block *StreamContentBlockStop) []*responses.ResponseChunk {
	var chunks []*responses.ResponseChunk

	// Infer block type from accumulated state if not set
	if c.currentBlockType == "" {
		if c.accumulatedArgs != "" {
			c.currentBlockType = "toolUse"
		} else if c.accumulatedSig != "" {
			c.currentBlockType = "reasoning"
		} else {
			c.currentBlockType = "text"
		}
	}

	if c.currentBlockType == "text" {
		// Ensure start events were emitted
		chunks = append(chunks, c.ensureTextBlockStarted()...)

		chunks = append(chunks, &responses.ResponseChunk{
			OfOutputTextDone: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDone]{
				Type:           constants.ChunkTypeOutputTextDone("response.output_text.done"),
				SequenceNumber: c.nextSeqNum(),
				ItemId:         c.currentOutputID,
				OutputIndex:    c.outputIndex,
				ContentIndex:   0,
				Text:           utils.Ptr(c.accumulatedText),
			},
		})

		chunks = append(chunks, c.buildContentPartDoneText()...)
		chunks = append(chunks, c.buildOutputItemDoneMessage()...)

		c.completedOutputs = append(c.completedOutputs, responses.OutputMessageUnion{
			OfOutputMessage: &responses.OutputMessage{
				ID:   c.currentOutputID,
				Role: constants.RoleAssistant,
				Content: &responses.OutputContent{
					{
						OfOutputText: &responses.OutputTextContent{
							Text:        c.accumulatedText,
							Annotations: []responses.Annotation{},
						},
					},
				},
			},
		})
	} else if c.currentBlockType == "toolUse" {
		// Ensure start events were emitted
		chunks = append(chunks, c.ensureToolUseBlockStarted()...)

		chunks = append(chunks, &responses.ResponseChunk{
			OfFunctionCallArgumentsDone: &responses.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDone]{
				Type:           constants.ChunkTypeFunctionCallArgumentsDone("response.function_call_arguments.done"),
				SequenceNumber: c.nextSeqNum(),
				ItemId:         c.currentOutputID,
				OutputIndex:    c.outputIndex,
				Arguments:      c.accumulatedArgs,
			},
		})

		chunks = append(chunks, c.buildOutputItemDoneFunctionCall()...)

		c.completedOutputs = append(c.completedOutputs, responses.OutputMessageUnion{
			OfFunctionCall: &responses.FunctionCallMessage{
				ID:        c.currentOutputID,
				CallID:    c.currentToolUseId,
				Name:      c.currentToolName,
				Arguments: c.accumulatedArgs,
			},
		})
	} else if c.currentBlockType == "reasoning" {
		// Ensure start events were emitted
		chunks = append(chunks, c.ensureReasoningBlockStarted()...)

		chunks = append(chunks, c.buildReasoningSummaryTextDone(c.accumulatedText))
		chunks = append(chunks, c.buildReasoningSummaryPartDone(c.accumulatedText))
		chunks = append(chunks, c.buildOutputItemDoneReasoning(c.accumulatedText, c.accumulatedSig))

		c.completedOutputs = append(c.completedOutputs, responses.OutputMessageUnion{
			OfReasoning: &responses.ReasoningMessage{
				ID:               c.currentOutputID,
				Summary:          []responses.SummaryTextContent{{Text: c.accumulatedText}},
				EncryptedContent: utils.Ptr(c.accumulatedSig),
			},
		})
	}

	c.outputIndex++
	c.blockStarted = false
	c.currentBlockType = ""
	c.currentOutputID = ""
	c.accumulatedText = ""
	c.accumulatedSig = ""

	return chunks
}

func (c *ConverseStreamToNativeConverter) handleMessageStop(msg *StreamMessageStop) []*responses.ResponseChunk {
	c.stopReason = msg.StopReason
	// Don't emit response.completed yet — wait for metadata event which carries usage
	return nil
}

func (c *ConverseStreamToNativeConverter) handleMetadata(meta *StreamMetadata) []*responses.ResponseChunk {
	if meta.Usage != nil {
		c.usage = nativeUsage(*meta.Usage)
	}

	return c.buildResponseCompleted()
}

func (c *ConverseStreamToNativeConverter) buildResponseCompleted() []*responses.ResponseChunk {
	status := "completed"
	if c.stopReason == "max_tokens" {
		status = "incomplete"
	}

	usage := responses.Usage{}
	if c.usage != nil {
		usage = *c.usage
	}

	return []*responses.ResponseChunk{
		{
			OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{
				Type:           constants.ChunkTypeResponseCompleted("response.completed"),
				SequenceNumber: c.nextSeqNum(),
				Response: responses.ChunkResponseData{
					Id:     c.responseID,
					Object: "response",
					Status: status,
					Output: c.completedOutputs,
					Usage:  usage,
					Request: responses.Request{
						Model: c.model,
					},
				},
			},
		},
	}
}

// =============================================================================
// Chunk Builders
// =============================================================================

func (c *ConverseStreamToNativeConverter) buildOutputItemAddedMessage() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded("response.output_item.added"),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:    "message",
				Id:      c.currentOutputID,
				Role:    constants.RoleAssistant,
				Status:  "in_progress",
				Content: &responses.ChunkOutputItemContent{},
			},
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildOutputItemAddedFunctionCall() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded("response.output_item.added"),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:   "function_call",
				Id:     c.currentOutputID,
				CallID: &c.currentToolUseId,
				Name:   &c.currentToolName,
				Status: "in_progress",
			},
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildContentPartAddedText() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfContentPartAdded: &responses.ChunkContentPart[constants.ChunkTypeContentPartAdded]{
			Type:           constants.ChunkTypeContentPartAdded("response.content_part.added"),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   0,
			Part: responses.ChunkOutputItemContentUnion{
				OfOutputText: &responses.OutputTextContent{
					Text:        "",
					Annotations: []responses.Annotation{},
				},
			},
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildContentPartDoneText() []*responses.ResponseChunk {
	return []*responses.ResponseChunk{
		{
			OfContentPartDone: &responses.ChunkContentPart[constants.ChunkTypeContentPartDone]{
				Type:           constants.ChunkTypeContentPartDone("response.content_part.done"),
				SequenceNumber: c.nextSeqNum(),
				ItemId:         c.currentOutputID,
				OutputIndex:    c.outputIndex,
				ContentIndex:   0,
				Part: responses.ChunkOutputItemContentUnion{
					OfOutputText: &responses.OutputTextContent{
						Text:        c.accumulatedText,
						Annotations: []responses.Annotation{},
					},
				},
			},
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildOutputItemDoneMessage() []*responses.ResponseChunk {
	return []*responses.ResponseChunk{
		{
			OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
				Type:           constants.ChunkTypeOutputItemDone("response.output_item.done"),
				SequenceNumber: c.nextSeqNum(),
				OutputIndex:    c.outputIndex,
				Item: responses.ChunkOutputItemData{
					Type:    "message",
					Id:      c.currentOutputID,
					Role:    constants.RoleAssistant,
					Status:  "completed",
					Content: &responses.ChunkOutputItemContent{{OfOutputText: &responses.OutputTextContent{Text: c.accumulatedText}}},
				},
			},
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildOutputItemDoneFunctionCall() []*responses.ResponseChunk {
	return []*responses.ResponseChunk{
		{
			OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
				Type:           constants.ChunkTypeOutputItemDone("response.output_item.done"),
				SequenceNumber: c.nextSeqNum(),
				OutputIndex:    c.outputIndex,
				Item: responses.ChunkOutputItemData{
					Type:      "function_call",
					Id:        c.currentOutputID,
					CallID:    utils.Ptr(c.currentToolUseId),
					Name:      utils.Ptr(c.currentToolName),
					Status:    "completed",
					Arguments: utils.Ptr(c.accumulatedArgs),
				},
			},
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildOutputItemAddedReasoning() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded("response.output_item.added"),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:             "reasoning",
				Id:               c.currentOutputID,
				Status:           "in_progress",
				Summary:          &[]responses.SummaryTextContent{},
				EncryptedContent: utils.Ptr(""),
			},
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildReasoningSummaryPartAdded() *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningSummaryPartAdded: &responses.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartAdded]{
			Type:           constants.ChunkTypeReasoningSummaryPartAdded("response.reasoning_summary_part.added"),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   0,
			Part:           responses.SummaryTextContent{Text: ""},
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildReasoningSummaryTextDelta(delta string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningSummaryTextDelta: &responses.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]{
			Type:           constants.ChunkTypeReasoningSummaryTextDelta("response.reasoning_summary_text.delta"),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   0,
			Delta:          delta,
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildReasoningSignatureDelta(sig string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningSummaryTextDelta: &responses.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]{
			Type:             constants.ChunkTypeReasoningSummaryTextDelta("response.reasoning_summary_text.delta"),
			SequenceNumber:   c.nextSeqNum(),
			ItemId:           c.currentOutputID,
			OutputIndex:      c.outputIndex,
			SummaryIndex:     0,
			EncryptedContent: utils.Ptr(sig),
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildReasoningSummaryTextDone(text string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningSummaryTextDone: &responses.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDone]{
			Type:           constants.ChunkTypeReasoningSummaryTextDone("response.reasoning_summary_text.done"),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   0,
			Text:           utils.Ptr(text),
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildReasoningSummaryPartDone(text string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfReasoningSummaryPartDone: &responses.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartDone]{
			Type:           constants.ChunkTypeReasoningSummaryPartDone("response.reasoning_summary_part.done"),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.currentOutputID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   0,
			Part:           responses.SummaryTextContent{Text: text},
		},
	}
}

func (c *ConverseStreamToNativeConverter) buildOutputItemDoneReasoning(text, sig string) *responses.ResponseChunk {
	return &responses.ResponseChunk{
		OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone("response.output_item.done"),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses.ChunkOutputItemData{
				Type:             "reasoning",
				Id:               c.currentOutputID,
				Status:           "completed",
				EncryptedContent: utils.Ptr(sig),
				Summary:          &[]responses.SummaryTextContent{{Text: text}},
			},
		},
	}
}
