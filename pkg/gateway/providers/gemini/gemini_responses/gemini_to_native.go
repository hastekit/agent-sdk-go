package gemini_responses

import (
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	responses2 "github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

func (in *Request) ToNativeRequest() *responses2.Request {
	out := &responses2.Request{
		Model:        in.Model,
		Input:        MessagesToNativeMessages(in.Contents),
		Instructions: utils.Ptr(in.SystemInstruction.String()),
		Tools:        ToolsToNativeTools(in.Tools),
		Parameters: responses2.Parameters{
			Background:        nil,
			MaxOutputTokens:   in.GenerationConfig.MaxOutputTokens,
			MaxToolCalls:      nil,
			ParallelToolCalls: nil,
			Store:             nil,
			Temperature:       in.GenerationConfig.Temperature,
			TopLogprobs:       in.GenerationConfig.TopK,
			TopP:              in.GenerationConfig.TopP,
			Include:           nil,
			Metadata:          nil,
			Stream:            in.Stream,
		},
	}

	includables := []responses2.Includable{}
	if in.GenerationConfig.ThinkingConfig != nil {
		effort := "high"
		if in.GenerationConfig.ThinkingConfig.ThinkingLevel != nil && *in.GenerationConfig.ThinkingConfig.ThinkingLevel != "HIGH" {
			effort = "low"
		}

		out.Reasoning = &responses2.ReasoningParam{
			Effort:  &effort,
			Summary: utils.Ptr("auto"),
		}

		includables = append(includables, responses2.IncludableReasoningEncryptedContent)
	}

	if len(includables) > 0 {
		out.Include = includables
	}

	if (out.Tools == nil || len(out.Tools) == 0) && in.GenerationConfig.ResponseJsonSchema != nil {
		out.Text = &responses2.TextFormat{
			Format: map[string]any{
				"type":   "json_schema",
				"name":   "structured_output",
				"strict": false,
				"schema": in.GenerationConfig.ResponseJsonSchema,
			},
		}
	}

	return out
}

func (in Role) ToNativeRole() constants.Role {
	switch in {
	case RoleUser:
		return constants.RoleUser
	case RoleModel:
		return constants.RoleAssistant
	case RoleSystem:
		return constants.RoleSystem
	}

	return constants.RoleAssistant
}

func ToolsToNativeTools(in []Tool) []responses2.ToolUnion {
	out := []responses2.ToolUnion{}

	for _, tool := range in {
		out = append(out, tool.ToNative()...)
	}

	return out
}

func (in *Tool) ToNative() []responses2.ToolUnion {
	out := []responses2.ToolUnion{}

	if in.FunctionDeclarations != nil {
		for _, fnDecl := range in.FunctionDeclarations {
			out = append(out, responses2.ToolUnion{
				OfFunction: &responses2.FunctionTool{
					Type:        "function",
					Name:        fnDecl.Name,
					Description: utils.Ptr(fnDecl.Description),
					Parameters:  fnDecl.ParametersJsonSchema,
				},
			})
		}
	}

	if in.CodeExecution != nil {
		out = append(out, responses2.ToolUnion{
			OfCodeExecution: &responses2.CodeExecutionTool{},
		})
	}

	return out
}

func MessagesToNativeMessages(msgs []Content) responses2.InputUnion {
	out := responses2.InputUnion{
		OfString:           nil,
		OfInputMessageList: responses2.InputMessageList{},
	}

	for _, content := range msgs {
		out.OfInputMessageList = append(out.OfInputMessageList, content.ToNativeMessage())
	}

	return out
}

func (content *Content) ToNativeMessage() responses2.InputMessageUnion {
	contents := responses2.InputContent{}
	var previousExecutableCodePart *ExecutableCodePart

	for _, part := range content.Parts {
		if part.FunctionCall != nil {
			args, err := sonic.Marshal(part.FunctionCall.Args)
			if err != nil {
				args = []byte("{}")
			}

			return responses2.InputMessageUnion{
				OfFunctionCall: &responses2.FunctionCallMessage{
					Name:      part.FunctionCall.Name,
					Arguments: string(args),
				},
			}
		}

		if part.FunctionResponse != nil {
			for _, v := range part.FunctionResponse.Response {
				return responses2.InputMessageUnion{
					OfFunctionCallOutput: &responses2.FunctionCallOutputMessage{
						ID:     part.FunctionResponse.ID,
						CallID: part.FunctionResponse.ID,
						Output: responses2.FunctionCallOutputContentUnion{
							OfString: utils.Ptr(v.(string)),
							OfList:   responses2.InputContent{},
						},
					},
				}
			}
		}

		if part.ExecutableCode != nil {
			previousExecutableCodePart = part.ExecutableCode
		}

		if previousExecutableCodePart != nil && part.CodeExecutionResult != nil {
			return responses2.InputMessageUnion{
				OfCodeInterpreterCall: &responses2.CodeInterpreterCallMessage{
					Status: "completed",
					Code:   previousExecutableCodePart.Code,
					Outputs: []responses2.CodeInterpreterCallOutputParam{
						{
							Type: "logs",
							Logs: part.CodeExecutionResult.Output,
						},
					},
				},
			}
		}

		if part.Text != nil {
			contents = append(contents, responses2.InputContentUnion{
				OfInputText: &responses2.InputTextContent{
					Type: "input_text",
					Text: *part.Text,
				},
			})
		}

		if part.InlineData != nil {
			if strings.HasPrefix(part.InlineData.MimeType, "image") {
				contents = append(contents, responses2.InputContentUnion{
					OfInputImage: &responses2.InputImageContent{
						ImageURL: utils.Ptr(fmt.Sprintf("data:%s;base64,%s", part.InlineData.MimeType, part.InlineData.Data)),
					},
				})
			}
		}
	}

	return responses2.InputMessageUnion{
		OfInputMessage: &responses2.InputMessage{
			Role:    content.Role.ToNativeRole(),
			Content: contents,
		},
	}
}

func (in *Response) ToNativeResponse() *responses2.Response {
	output := []responses2.OutputMessageUnion{}

	// Gemini can return an empty candidate list (e.g. the prompt was
	// blocked by a safety filter, or the response only carries
	// promptFeedback). Guard every Candidates[0] access so a missing
	// candidate yields an empty response rather than a panic.
	var stopReason any
	if len(in.Candidates) == 0 {
		return &responses2.Response{
			ID:       in.ResponseID,
			Model:    in.ModelVersion,
			Output:   output,
			Usage:    utils.Ptr(nativeUsage(in.UsageMetadata)),
			Metadata: map[string]any{"stop_reason": stopReason},
		}
	}

	var previousExecutableCodePart *ExecutableCodePart
	for _, part := range in.Candidates[0].Content.Parts {
		if part.Text != nil {
			output = append(output, responses2.OutputMessageUnion{
				OfOutputMessage: &responses2.OutputMessage{
					Role: constants.RoleAssistant,
					Content: &responses2.OutputContent{
						{
							OfOutputText: &responses2.OutputTextContent{
								Text: *part.Text,
							},
						},
					},
				},
			})
		}

		if part.FunctionCall != nil {
			args, err := sonic.Marshal(part.FunctionCall.Args)
			if err != nil {
				args = []byte("{}")
			}

			callId := uuid.NewString()
			output = append(output, responses2.OutputMessageUnion{
				OfFunctionCall: &responses2.FunctionCallMessage{
					ID:        callId,
					CallID:    callId,
					Name:      part.FunctionCall.Name,
					Arguments: string(args),
				},
			})
		}

		if part.ExecutableCode != nil {
			previousExecutableCodePart = part.ExecutableCode
		}

		if part.CodeExecutionResult != nil && previousExecutableCodePart != nil {
			output = append(output, responses2.OutputMessageUnion{
				OfCodeInterpreterCall: &responses2.CodeInterpreterCallMessage{
					Code: previousExecutableCodePart.Code,
					Outputs: []responses2.CodeInterpreterCallOutputParam{
						{
							Type: "logs",
							Logs: part.CodeExecutionResult.Output,
						},
					},
				},
			})
			previousExecutableCodePart = nil
		}
	}

	return &responses2.Response{
		ID:          in.ResponseID,
		Model:       in.ModelVersion,
		Output:      output,
		Usage:       utils.Ptr(nativeUsage(in.UsageMetadata)),
		Error:       nil,
		ServiceTier: "",
		Metadata: map[string]any{
			"stop_reason": in.Candidates[0].FinishReason,
		},
	}
}

// nativeUsage normalizes Gemini's token accounting onto the native Usage
// contract. Two Gemini-specific quirks:
//
//   - promptTokenCount already includes cachedContentTokenCount, so the cached
//     count maps straight to the details field with nothing added on top.
//     Gemini is the only provider here that does this — Anthropic and Bedrock
//     both report cache counts outside their prompt total and have to add them.
//   - candidatesTokenCount excludes thoughtsTokenCount, whereas the contract
//     treats reasoning as a subset of OutputTokens. Adding them recovers the
//     real output size and makes InputTokens+OutputTokens agree with the
//     totalTokenCount Gemini reports, which counts thoughts.
func nativeUsage(u *UsageMetadata) responses2.Usage {
	if u == nil {
		return responses2.Usage{}
	}

	out := responses2.Usage{
		InputTokens:  u.PromptTokenCount,
		OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
	}
	out.InputTokensDetails.CachedTokens = u.CachedContentTokenCount
	out.OutputTokensDetails.ReasoningTokens = u.ThoughtsTokenCount
	out.TotalTokens = out.InputTokens + out.OutputTokens

	return out
}

// =============================================================================
// Gemini ResponseChunk to Native Conversion
// =============================================================================

// ResponseChunkToNativeResponseChunkConverter converts Gemini stream chunks to native format.
// Gemini streams parts within Response objects, unlike Anthropic's event-based streaming.
type ResponseChunkToNativeResponseChunkConverter struct {
	// Stream lifecycle
	streamStarted bool
	streamEnded   bool

	// Current output item state
	currentBlock     *Part
	outputItemActive bool
	outputItemID     string
	outputIndex      int
	contentIndex     int

	// For detecting content type transitions
	previousPart *Part

	// Accumulation
	accumulatedData  string
	completedOutputs []responses2.OutputMessageUnion

	// Message-level state
	sequenceNumber int
	messageID      string
	usage          UsageMetadata
	model          string
}

// nextSeqNum returns the next sequence number and increments the counter.
func (c *ResponseChunkToNativeResponseChunkConverter) nextSeqNum() int {
	n := c.sequenceNumber
	c.sequenceNumber++
	return n
}

// getPartType returns the type of a part for transition detection.
func (c *ResponseChunkToNativeResponseChunkConverter) getPartType(part *Part) string {
	switch {
	case part.Text != nil:
		if part.IsThought() {
			return "thought"
		}
		return "text"
	case part.FunctionCall != nil:
		return "function_call"
	case part.InlineData != nil:
		if strings.HasPrefix(part.InlineData.MimeType, "image") {
			return "image_generation_call"
		}
	case part.ExecutableCode != nil && part.CodeExecutionResult != nil:
		return "code_execution"
	}

	return ""
}

// ResponseChunkToNativeResponseChunk converts a Gemini response chunk to native format.
// Pass nil to signal end of stream and emit completion events.
func (c *ResponseChunkToNativeResponseChunkConverter) ResponseChunkToNativeResponseChunk(in *Response) []*responses2.ResponseChunk {
	// Stream already ended, ignore further input
	if c.streamEnded {
		return nil
	}

	// nil input signals end of stream
	if in == nil {
		return c.handleStreamEnd()
	}

	// Update usage and model from each chunk (Gemini sends these with every chunk)
	if in.UsageMetadata != nil {
		c.usage = *in.UsageMetadata
	}
	c.model = in.ResponseID

	var out []*responses2.ResponseChunk

	// Emit stream start events on first chunk
	if !c.streamStarted {
		out = append(out, c.emitStreamStart(in)...)
	}

	// Process all parts in this chunk. A chunk may carry no candidate
	// (e.g. it only updates usage or prompt feedback), so guard the access.
	if len(in.Candidates) > 0 {
		for i := range in.Candidates[0].Content.Parts {
			part := &in.Candidates[0].Content.Parts[i]
			out = append(out, c.handlePart(part)...)
		}
	}

	return out
}

// =============================================================================
// Stream Lifecycle Handlers
// =============================================================================

func (c *ResponseChunkToNativeResponseChunkConverter) emitStreamStart(in *Response) []*responses2.ResponseChunk {
	c.streamStarted = true
	c.messageID = in.ResponseID

	return []*responses2.ResponseChunk{
		c.buildResponseCreated(in.ResponseID, in.ModelVersion),
		c.buildResponseInProgress(in.ResponseID),
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) handleStreamEnd() []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	// Complete any active output item
	if c.previousPart != nil {
		out = append(out, c.completeCurrentPart()...)
	}

	// Emit response.completed
	out = append(out, c.buildResponseCompleted())
	c.streamEnded = true

	return out
}

// =============================================================================
// Part Handlers
// =============================================================================

func (c *ResponseChunkToNativeResponseChunkConverter) handlePart(part *Part) []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	// Check if it is an empty part
	if part.Text != nil && *part.Text == "" {
		return []*responses2.ResponseChunk{}
	}

	// Check if we need to complete previous part (content type changed)
	if c.shouldEndPreviousPart(part) {
		out = append(out, c.completeCurrentPart()...)
		c.outputItemActive = false
		c.accumulatedData = ""
	}

	// Store current block for later reference (used in completion)
	if !c.outputItemActive {
		c.currentBlock = part
		c.outputItemID = uuid.NewString()
	}

	// Handle based on part type
	switch {
	case part.Text != nil:
		if part.IsThought() {
			out = append(out, c.handleThoughtPart(part)...)
		} else {
			out = append(out, c.handleTextPart(part)...)
		}
	case part.FunctionCall != nil:
		out = append(out, c.handleFunctionCallPart(part)...)

	case part.InlineData != nil:
		if strings.HasPrefix(part.InlineData.MimeType, "image") {
			out = append(out, c.handleInlineImageDataPart(part)...)
		}

	case part.ExecutableCode != nil:
		out = append(out, c.handleExecutableCodePart(part)...)

	case part.CodeExecutionResult != nil:
		out = append(out, c.handleCodeExecutionResultPart(part)...)
	}

	c.outputItemActive = true
	c.previousPart = part

	return out
}

func (c *ResponseChunkToNativeResponseChunkConverter) shouldEndPreviousPart(part *Part) bool {
	if c.previousPart == nil {
		return false
	}
	return c.getPartType(c.previousPart) != c.getPartType(part)
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeCurrentPart() []*responses2.ResponseChunk {
	if c.previousPart == nil {
		return nil
	}

	switch {
	case c.previousPart.Text != nil:
		if c.previousPart.IsThought() {
			return c.completeThoughtPart()
		} else {
			return c.completeTextPart()
		}
	case c.previousPart.FunctionCall != nil:
		return c.completeFunctionCallPart()

	case c.previousPart.InlineData != nil:
		if strings.HasPrefix(c.previousPart.InlineData.MimeType, "image") {
			return c.completeInlineImageDataPart()
		}

	case c.previousPart.CodeExecutionResult != nil:
		return c.completeCodeExecutionResult()
	}

	return nil
}

// =============================================================================
// Text Part Handling
// =============================================================================

func (c *ResponseChunkToNativeResponseChunkConverter) handleTextPart(part *Part) []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	// Emit start events if this is a new output item
	if !c.outputItemActive {
		out = append(out,
			c.buildOutputItemAddedMessage(),
			c.buildContentPartAddedText(),
		)
	}

	// Avoid emitting empty text if thought signature is present
	if (part.Text == nil || *part.Text == "") && (part.ThoughtSignature != nil && *part.ThoughtSignature != "") {
		return out
	}

	// Emit delta
	out = append(out, c.buildOutputTextDelta(*part.Text))
	c.accumulatedData += *part.Text

	return out
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeTextPart() []*responses2.ResponseChunk {
	text := c.accumulatedData

	// Store completed output for final response
	c.completedOutputs = append(c.completedOutputs, responses2.OutputMessageUnion{
		OfOutputMessage: &responses2.OutputMessage{
			ID:   c.outputItemID,
			Role: RoleModel.ToNativeRole(),
			Content: &responses2.OutputContent{
				{OfOutputText: &responses2.OutputTextContent{Text: text}},
			},
		},
	})

	return []*responses2.ResponseChunk{
		c.buildOutputTextDone(text),
		c.buildContentPartDoneText(text),
		c.buildOutputItemDoneMessage(text),
	}
}

// =============================================================================
// Function Call Part Handling
// =============================================================================

func (c *ResponseChunkToNativeResponseChunkConverter) handleFunctionCallPart(part *Part) []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	args, _ := sonic.Marshal(part.FunctionCall.Args)
	if args == nil {
		args = []byte("{}")
	}
	argsStr := string(args)

	// Emit start events if this is a new output item
	if !c.outputItemActive {
		callID := uuid.NewString() + "_" + part.FunctionCall.Name
		out = append(out, c.buildOutputItemAddedFunctionCall(callID, part.FunctionCall.Name, argsStr, part.ThoughtSignature))
	}

	// Emit delta
	out = append(out, c.buildFunctionCallArgumentsDelta(argsStr))
	c.accumulatedData += argsStr

	return out
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeFunctionCallPart() []*responses2.ResponseChunk {
	args := c.accumulatedData
	if args == "" {
		args = "{}"
	}

	callID := uuid.NewString()
	fnName := c.currentBlock.FunctionCall.Name

	// Store completed output for final response
	c.completedOutputs = append(c.completedOutputs, responses2.OutputMessageUnion{
		OfFunctionCall: &responses2.FunctionCallMessage{
			ID:        c.outputItemID,
			CallID:    callID,
			Name:      fnName,
			Arguments: args,
		},
	})

	return []*responses2.ResponseChunk{
		c.buildFunctionCallArgumentsDone(args),
		c.buildOutputItemDoneFunctionCall(callID, fnName, args),
	}
}

// =============================================================================
// Thought Part Handling
// =============================================================================

func (c *ResponseChunkToNativeResponseChunkConverter) handleThoughtPart(part *Part) []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	// Emit start events if this is a new output item
	if !c.outputItemActive {
		out = append(out,
			c.buildOutputItemAddedReasoning(),
			c.buildReasoningSummaryPartAdded(),
		)
	}

	// Emit delta
	out = append(out, c.buildReasoningSummaryTextDelta(*part.Text))
	c.accumulatedData += *part.Text

	return out
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeThoughtPart() []*responses2.ResponseChunk {
	text := c.accumulatedData

	// Store completed output for final response
	c.completedOutputs = append(c.completedOutputs, responses2.OutputMessageUnion{
		OfReasoning: &responses2.ReasoningMessage{
			ID: c.outputItemID,
			Summary: []responses2.SummaryTextContent{
				{Text: text},
			},
			EncryptedContent: nil,
		},
	})

	return []*responses2.ResponseChunk{
		c.buildReasoningSummaryTextDone(text),
		c.buildReasoningSummaryPartDone(text),
		c.buildOutputItemDoneReasoningSummary(text),
	}
}

// =============================================================================
// Inline Data Handling
// =============================================================================

func (c *ResponseChunkToNativeResponseChunkConverter) handleInlineImageDataPart(part *Part) []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	// Emit start events if this is a new output item
	if !c.outputItemActive {
		out = append(out,
			c.buildOutputItemAddedImageGenerationCall(),
			c.buildImageGenerationCallInProgress(),
			c.buildImageGenerationCallGenerating(),
		)
	}

	// Emit delta
	out = append(out, c.buildImageGenerationCallPartialImage(part.InlineData.MimeType, part.InlineData.Data))
	c.accumulatedData = part.InlineData.Data

	return out
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeInlineImageDataPart() []*responses2.ResponseChunk {
	// Store completed output for final response
	c.completedOutputs = append(c.completedOutputs, responses2.OutputMessageUnion{
		OfImageGenerationCall: &responses2.ImageGenerationCallMessage{
			ID:           c.outputItemID,
			Status:       "completed",
			OutputFormat: strings.TrimPrefix(c.currentBlock.InlineData.MimeType, "image/"),
			Result:       c.accumulatedData,
			Background:   "",
			Quality:      "",
			Size:         "",
		},
	})

	return []*responses2.ResponseChunk{
		c.buildOutputItemDoneImageGenerationCall(c.currentBlock.InlineData.MimeType, c.currentBlock.InlineData.Data),
	}
}

// =============================================================================
// Code Execution Handling
// =============================================================================

func (c *ResponseChunkToNativeResponseChunkConverter) handleExecutableCodePart(part *Part) []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	// output_item.added
	out = append(out, c.buildOutputItemAddedCodeInterpreterCall(""))
	// code_interpreter_call.in_progress
	out = append(out, c.buildCodeInterpreterCallInProgress())
	// code_interpreter_call_code.delta
	out = append(out, c.buildCodeInterpreterCallCodeDelta(part.ExecutableCode.Code))
	// code_interpreter_call_code.done
	out = append(out, c.buildCodeInterpreterCallCodeDone(part.ExecutableCode.Code, part.ThoughtSignature))
	// code_interpreter_call.interpreting
	out = append(out, c.buildCodeInterpreterCallInterpreting())

	// Store the code
	c.accumulatedData = part.ExecutableCode.Code

	return out
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeExecutableCodePart() []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	// Store the code
	c.accumulatedData = c.currentBlock.ExecutableCode.Code

	return out
}

func (c *ResponseChunkToNativeResponseChunkConverter) handleCodeExecutionResultPart(_ *Part) []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	// Do nothing

	return out
}

func (c *ResponseChunkToNativeResponseChunkConverter) completeCodeExecutionResult() []*responses2.ResponseChunk {
	var out []*responses2.ResponseChunk

	// code_interpreter_call.completed
	out = append(out, c.buildCodeInterpreterCallCompleted(c.accumulatedData))

	// output_item.done
	out = append(out, c.buildOutputItemDoneCodeInterpreterCall(c.accumulatedData, c.previousPart.CodeExecutionResult.Output))

	return out
}

// =============================================================================
// Chunk Builders
// =============================================================================

func (c *ResponseChunkToNativeResponseChunkConverter) buildResponseCreated(id, model string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfResponseCreated: &responses2.ChunkResponse[constants.ChunkTypeResponseCreated]{
			Type:           constants.ChunkTypeResponseCreated(""),
			SequenceNumber: c.nextSeqNum(),
			Response: responses2.ChunkResponseData{
				Id:         id,
				Object:     "response",
				CreatedAt:  int(time.Now().Unix()),
				Status:     "in_progress",
				Background: false,
				Request:    responses2.Request{Model: model},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildResponseInProgress(id string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfResponseInProgress: &responses2.ChunkResponse[constants.ChunkTypeResponseInProgress]{
			Type:           constants.ChunkTypeResponseInProgress(""),
			SequenceNumber: c.nextSeqNum(),
			Response: responses2.ChunkResponseData{
				Id:         id,
				Object:     "response",
				CreatedAt:  int(time.Now().Unix()),
				Status:     "in_progress",
				Background: false,
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedMessage() *responses2.ResponseChunk {
	c.outputItemID = responses2.NewOutputItemMessageID()

	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    0,
			Item: responses2.ChunkOutputItemData{
				Type:    "message",
				Id:      c.outputItemID,
				Status:  "in_progress",
				Role:    RoleModel.ToNativeRole(),
				Content: &responses2.ChunkOutputItemContent{},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedFunctionCall(callID, name, args string, thoughtSignature *string) *responses2.ResponseChunk {
	c.outputItemID = responses2.NewOutputItemFunctionCallID()

	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    0,
			Item: responses2.ChunkOutputItemData{
				Type:             "function_call",
				Id:               c.outputItemID,
				Status:           "in_progress",
				CallID:           utils.Ptr(callID),
				Name:             utils.Ptr(name),
				Arguments:        utils.Ptr(args),
				ThoughtSignature: thoughtSignature, // Special case: gemini can have thought signature of function calls
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedReasoning() *responses2.ResponseChunk {
	c.outputItemID = responses2.NewOutputItemReasoningID()

	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:             "reasoning",
				Id:               c.outputItemID,
				Status:           "in_progress",
				Summary:          &[]responses2.SummaryTextContent{},
				EncryptedContent: utils.Ptr(""),
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildContentPartAddedText() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfContentPartAdded: &responses2.ChunkContentPart[constants.ChunkTypeContentPartAdded]{
			Type:           constants.ChunkTypeContentPartAdded(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   c.contentIndex,
			Part:           responses2.ChunkOutputItemContentUnion{OfOutputText: &responses2.OutputTextContent{Text: ""}},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputTextDelta(delta string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputTextDelta: &responses2.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{
			Type:           constants.ChunkTypeOutputTextDelta(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   c.contentIndex,
			Delta:          delta,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildFunctionCallArgumentsDelta(delta string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfFunctionCallArgumentsDelta: &responses2.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]{
			Type:           constants.ChunkTypeFunctionCallArgumentsDelta(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			Delta:          delta,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputTextDone(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputTextDone: &responses2.ChunkOutputText[constants.ChunkTypeOutputTextDone]{
			Type:           constants.ChunkTypeOutputTextDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   c.contentIndex,
			Text:           utils.Ptr(text),
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildContentPartDoneText(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfContentPartDone: &responses2.ChunkContentPart[constants.ChunkTypeContentPartDone]{
			Type:           constants.ChunkTypeContentPartDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			ContentIndex:   c.contentIndex,
			Part:           responses2.ChunkOutputItemContentUnion{OfOutputText: &responses2.OutputTextContent{Text: text}},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneMessage(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    0,
			Item: responses2.ChunkOutputItemData{
				Type:    "message",
				Id:      c.outputItemID,
				Status:  "completed",
				Role:    RoleModel.ToNativeRole(),
				Content: &responses2.ChunkOutputItemContent{{OfOutputText: &responses2.OutputTextContent{Text: text}}},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildFunctionCallArgumentsDone(args string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfFunctionCallArgumentsDone: &responses2.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDone]{
			Type:           constants.ChunkTypeFunctionCallArgumentsDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			Arguments:      args,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneFunctionCall(callID, name, args string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    0,
			Item: responses2.ChunkOutputItemData{
				Type:             "function_call",
				Id:               c.outputItemID,
				Status:           "completed",
				CallID:           utils.Ptr(callID),
				Name:             utils.Ptr(name),
				Arguments:        utils.Ptr(args),
				ThoughtSignature: c.previousPart.ThoughtSignature,
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildReasoningSummaryPartAdded() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryPartAdded: &responses2.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartAdded]{
			Type:           constants.ChunkTypeReasoningSummaryPartAdded(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   c.contentIndex,
			Part:           responses2.SummaryTextContent{Text: ""},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildReasoningSummaryTextDelta(delta string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryTextDelta: &responses2.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]{
			Type:           constants.ChunkTypeReasoningSummaryTextDelta(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   c.contentIndex,
			Delta:          delta,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildReasoningSummaryTextDone(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryTextDone: &responses2.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDone]{
			Type:           constants.ChunkTypeReasoningSummaryTextDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   c.contentIndex,
			Text:           utils.Ptr(text),
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildReasoningSummaryPartDone(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryPartDone: &responses2.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartDone]{
			Type:           constants.ChunkTypeReasoningSummaryPartDone(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			SummaryIndex:   c.contentIndex,
			Part: responses2.SummaryTextContent{
				Text: text,
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneReasoningSummary(text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    0,
			Item: responses2.ChunkOutputItemData{
				Type:    "reasoning",
				Id:      c.outputItemID,
				Status:  "completed",
				Summary: &[]responses2.SummaryTextContent{{Text: text}},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedImageGenerationCall() *responses2.ResponseChunk {
	c.outputItemID = responses2.NewOutputItemReasoningID()

	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:   "image_generation_call",
				Id:     c.outputItemID,
				Status: "in_progress",

				Background:   utils.Ptr(""),
				Result:       utils.Ptr(""),
				Size:         utils.Ptr(""),
				OutputFormat: utils.Ptr(""),
				Quality:      utils.Ptr(""),
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildImageGenerationCallInProgress() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfImageGenerationCallInProgress: &responses2.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallInProgress]{
			Type:               constants.ChunkTypeImageGenerationCallInProgress(""),
			SequenceNumber:     c.nextSeqNum(),
			ItemId:             c.outputItemID,
			OutputIndex:        c.outputIndex,
			PartialImageIndex:  c.contentIndex,
			PartialImageBase64: "",
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildImageGenerationCallGenerating() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfImageGenerationCallGenerating: &responses2.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallGenerating]{
			Type:               constants.ChunkTypeImageGenerationCallGenerating(""),
			SequenceNumber:     c.nextSeqNum(),
			ItemId:             c.outputItemID,
			OutputIndex:        c.outputIndex,
			PartialImageIndex:  c.contentIndex,
			PartialImageBase64: "",
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildImageGenerationCallPartialImage(outputFormat string, imageData string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfImageGenerationCallPartialImage: &responses2.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{
			Type:               constants.ChunkTypeImageGenerationCallPartialImage(""),
			SequenceNumber:     c.nextSeqNum(),
			ItemId:             c.outputItemID,
			OutputIndex:        c.outputIndex,
			PartialImageIndex:  c.contentIndex,
			PartialImageBase64: imageData,
			OutputFormat:       utils.Ptr(strings.TrimPrefix(outputFormat, "image/")),

			// Following cannot be mapped
			Background: utils.Ptr(""),
			Quality:    utils.Ptr(""),
			Size:       utils.Ptr(""),
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneImageGenerationCall(outputFormat string, imageData string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    0,
			Item: responses2.ChunkOutputItemData{
				Type:   "image_generation_call",
				Id:     c.outputItemID,
				Status: "completed",

				Background:   utils.Ptr(""),
				Size:         utils.Ptr(""),
				Quality:      utils.Ptr(""),
				OutputFormat: utils.Ptr(strings.TrimPrefix(outputFormat, "image/")),
				Result:       utils.Ptr(imageData),
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildResponseCompleted() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfResponseCompleted: &responses2.ChunkResponse[constants.ChunkTypeResponseCompleted]{
			Type:           constants.ChunkTypeResponseCompleted(""),
			SequenceNumber: c.nextSeqNum(),
			Response: responses2.ChunkResponseData{
				Id:        c.messageID,
				Object:    "response",
				CreatedAt: int(time.Now().Unix()),
				Status:    "completed",
				Output:    c.completedOutputs,
				Usage:     nativeUsage(&c.usage),
				Request:   responses2.Request{Model: c.model},
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemAddedCodeInterpreterCall(code string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:   "code_interpreter_call",
				Id:     c.outputItemID,
				Status: "in_progress",
				Code:   &code,
			},
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallInProgress() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallInProgress: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallInProgress]{
			Type:           constants.ChunkTypeCodeInterpreterCallInProgress(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallCodeDelta(delta string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallCodeDelta: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDelta]{
			Type:           constants.ChunkTypeCodeInterpreterCallCodeDelta(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			Delta:          &delta,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallCodeDone(code string, thoughtSignature *string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallCodeDone: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCodeDone]{
			Type:             constants.ChunkTypeCodeInterpreterCallCodeDone(""),
			SequenceNumber:   c.nextSeqNum(),
			ItemId:           c.outputItemID,
			OutputIndex:      c.outputIndex,
			Code:             &code,
			ThoughtSignature: thoughtSignature,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallInterpreting() *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallInterpreting: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallInterpreting]{
			Type:           constants.ChunkTypeCodeInterpreterCallInterpreting(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildCodeInterpreterCallCompleted(code string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfCodeInterpreterCallCompleted: &responses2.ChunkCodeInterpreterCall[constants.ChunkTypeCodeInterpreterCallCompleted]{
			Type:           constants.ChunkTypeCodeInterpreterCallCompleted(""),
			SequenceNumber: c.nextSeqNum(),
			ItemId:         c.outputItemID,
			OutputIndex:    c.outputIndex,
			Code:           &code,
		},
	}
}

func (c *ResponseChunkToNativeResponseChunkConverter) buildOutputItemDoneCodeInterpreterCall(code string, output string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone(""),
			SequenceNumber: c.nextSeqNum(),
			OutputIndex:    c.outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:    "code_interpreter_call",
				Id:      c.outputItemID,
				Status:  "completed",
				Code:    &code,
				Outputs: []responses2.CodeInterpreterCallOutputParam{{Type: "logs", Logs: output}},
			},
		},
	}
}
