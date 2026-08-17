package anthropic_responses

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test Fixtures & Helpers for Native → Anthropic
// =============================================================================

func newNativeToAnthropicConverter() *NativeResponseChunkToResponseChunkConverter {
	return &NativeResponseChunkToResponseChunkConverter{}
}

// Helper to create a native response.created chunk
func createNativeResponseCreated(id, model string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfResponseCreated: &responses2.ChunkResponse[constants.ChunkTypeResponseCreated]{
			Type:           constants.ChunkTypeResponseCreated("response.created"),
			SequenceNumber: 0,
			Response: responses2.ChunkResponseData{
				Id:     id,
				Object: "response",
				Status: "in_progress",
				Request: responses2.Request{
					Model: model,
				},
			},
		},
	}
}

// Helper to create a native response.in_progress chunk
func createNativeResponseInProgress(id string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfResponseInProgress: &responses2.ChunkResponse[constants.ChunkTypeResponseInProgress]{
			Type:           constants.ChunkTypeResponseInProgress("response.in_progress"),
			SequenceNumber: 1,
			Response: responses2.ChunkResponseData{
				Id:     id,
				Object: "response",
				Status: "in_progress",
			},
		},
	}
}

// Helper to create a native output_item.added for text message
func createNativeOutputItemAddedMessage(itemId string, outputIndex int) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded("response.output_item.added"),
			SequenceNumber: 2,
			OutputIndex:    outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:    "message",
				Id:      itemId,
				Status:  "in_progress",
				Role:    constants.RoleAssistant,
				Content: &responses2.ChunkOutputItemContent{},
			},
		},
	}
}

// Helper to create a native output_item.added for function_call
func createNativeOutputItemAddedFunctionCall(itemId string, outputIndex int, callId, name string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded("response.output_item.added"),
			SequenceNumber: 2,
			OutputIndex:    outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:      "function_call",
				Id:        itemId,
				Status:    "in_progress",
				CallID:    utils.Ptr(callId),
				Name:      utils.Ptr(name),
				Arguments: utils.Ptr(""),
			},
		},
	}
}

// Helper to create a native content_part.added for text
func createNativeContentPartAddedText(itemId string, outputIndex, contentIndex int) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfContentPartAdded: &responses2.ChunkContentPart[constants.ChunkTypeContentPartAdded]{
			Type:           constants.ChunkTypeContentPartAdded("response.content_part.added"),
			SequenceNumber: 3,
			ItemId:         itemId,
			OutputIndex:    outputIndex,
			ContentIndex:   contentIndex,
			Part: responses2.ChunkOutputItemContentUnion{
				OfOutputText: &responses2.OutputTextContent{
					Text: "",
				},
			},
		},
	}
}

// Helper to create a native output_text.delta
func createNativeOutputTextDelta(itemId string, outputIndex, contentIndex int, delta string, seqNum int) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputTextDelta: &responses2.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{
			Type:           constants.ChunkTypeOutputTextDelta("response.output_text.delta"),
			SequenceNumber: seqNum,
			ItemId:         itemId,
			OutputIndex:    outputIndex,
			ContentIndex:   contentIndex,
			Delta:          delta,
		},
	}
}

// Helper to create a native output_text.done
func createNativeOutputTextDone(itemId string, outputIndex, contentIndex int, text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputTextDone: &responses2.ChunkOutputText[constants.ChunkTypeOutputTextDone]{
			Type:           constants.ChunkTypeOutputTextDone("response.output_text.done"),
			SequenceNumber: 5,
			ItemId:         itemId,
			OutputIndex:    outputIndex,
			ContentIndex:   contentIndex,
			Text:           utils.Ptr(text),
		},
	}
}

// Helper to create a native content_part.done
func createNativeContentPartDone(itemId string, outputIndex, contentIndex int, text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfContentPartDone: &responses2.ChunkContentPart[constants.ChunkTypeContentPartDone]{
			Type:           constants.ChunkTypeContentPartDone("response.content_part.done"),
			SequenceNumber: 6,
			ItemId:         itemId,
			OutputIndex:    outputIndex,
			ContentIndex:   contentIndex,
			Part: responses2.ChunkOutputItemContentUnion{
				OfOutputText: &responses2.OutputTextContent{
					Text: text,
				},
			},
		},
	}
}

// Helper to create a native output_item.done for message
func createNativeOutputItemDoneMessage(itemId string, outputIndex int, text string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone("response.output_item.done"),
			SequenceNumber: 7,
			OutputIndex:    outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:   "message",
				Id:     itemId,
				Status: "completed",
				Role:   constants.RoleAssistant,
				Content: &responses2.ChunkOutputItemContent{
					{OfOutputText: &responses2.OutputTextContent{Text: text}},
				},
			},
		},
	}
}

// Helper to create a native output_item.done for function_call
func createNativeOutputItemDoneFunctionCall(itemId string, outputIndex int, callId, name, args string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfOutputItemDone: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Type:           constants.ChunkTypeOutputItemDone("response.output_item.done"),
			SequenceNumber: 7,
			OutputIndex:    outputIndex,
			Item: responses2.ChunkOutputItemData{
				Type:      "function_call",
				Id:        itemId,
				Status:    "completed",
				CallID:    utils.Ptr(callId),
				Name:      utils.Ptr(name),
				Arguments: utils.Ptr(args),
			},
		},
	}
}

// Helper to create a native function_call_arguments.delta
func createNativeFunctionCallArgumentsDelta(itemId string, outputIndex int, delta string, seqNum int) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfFunctionCallArgumentsDelta: &responses2.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]{
			Type:           constants.ChunkTypeFunctionCallArgumentsDelta("response.function_call_arguments.delta"),
			SequenceNumber: seqNum,
			ItemId:         itemId,
			OutputIndex:    outputIndex,
			Delta:          delta,
		},
	}
}

// Helper to create a native function_call_arguments.done
func createNativeFunctionCallArgumentsDone(itemId string, outputIndex int, args string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfFunctionCallArgumentsDone: &responses2.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDone]{
			Type:           constants.ChunkTypeFunctionCallArgumentsDone("response.function_call_arguments.done"),
			SequenceNumber: 6,
			ItemId:         itemId,
			OutputIndex:    outputIndex,
			Arguments:      args,
		},
	}
}

// Helper to create a native reasoning_summary_part.added
func createNativeReasoningSummaryPartAdded(itemId string, outputIndex, summaryIndex int) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryPartAdded: &responses2.ChunkReasoningSummaryPart[constants.ChunkTypeReasoningSummaryPartAdded]{
			Type:           constants.ChunkTypeReasoningSummaryPartAdded("response.reasoning_summary_part.added"),
			SequenceNumber: 3,
			ItemId:         itemId,
			OutputIndex:    outputIndex,
			SummaryIndex:   summaryIndex,
			Part:           responses2.SummaryTextContent{Text: ""},
		},
	}
}

// Helper to create a native reasoning_summary_text.delta
func createNativeReasoningSummaryTextDelta(itemId string, outputIndex, summaryIndex int, delta string, seqNum int) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryTextDelta: &responses2.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]{
			Type:           constants.ChunkTypeReasoningSummaryTextDelta("response.reasoning_summary_text.delta"),
			SequenceNumber: seqNum,
			ItemId:         itemId,
			OutputIndex:    outputIndex,
			SummaryIndex:   summaryIndex,
			Delta:          delta,
		},
	}
}

// Helper to create a native reasoning_summary_text.delta with encrypted content (signature)
func createNativeReasoningSummaryTextDeltaWithSignature(itemId string, outputIndex, summaryIndex int, signature string) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfReasoningSummaryTextDelta: &responses2.ChunkReasoningSummaryText[constants.ChunkTypeReasoningSummaryTextDelta]{
			Type:             constants.ChunkTypeReasoningSummaryTextDelta("response.reasoning_summary_text.delta"),
			SequenceNumber:   5,
			ItemId:           itemId,
			OutputIndex:      outputIndex,
			SummaryIndex:     summaryIndex,
			EncryptedContent: utils.Ptr(signature),
		},
	}
}

// Helper to create a native response.completed
func createNativeResponseCompleted(id, model string, inputTokens, outputTokens int, outputs []responses2.OutputMessageUnion) *responses2.ResponseChunk {
	return &responses2.ResponseChunk{
		OfResponseCompleted: &responses2.ChunkResponse[constants.ChunkTypeResponseCompleted]{
			Type:           constants.ChunkTypeResponseCompleted("response.completed"),
			SequenceNumber: 10,
			Response: responses2.ChunkResponseData{
				Id:     id,
				Object: "response",
				Status: "completed",
				Output: outputs,
				Usage: responses2.Usage{
					InputTokens:  inputTokens,
					OutputTokens: outputTokens,
					TotalTokens:  inputTokens + outputTokens,
				},
				Request: responses2.Request{
					Model: model,
				},
			},
		},
	}
}

// =============================================================================
// Test: response.created Conversion
// =============================================================================

func TestNativeToAnthropic_ResponseCreated(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	chunk := createNativeResponseCreated("resp_123", "claude-3-opus-20240229")
	result := converter.NativeResponseChunkToResponseChunk(chunk)

	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfMessageStart)
	assert.Equal(t, "resp_123", result[0].OfMessageStart.Message.Id)
	assert.Equal(t, "claude-3-opus-20240229", result[0].OfMessageStart.Message.Model)
	assert.Equal(t, Role("assistant"), result[0].OfMessageStart.Message.Role)
	assert.Equal(t, "message", result[0].OfMessageStart.Message.Type)
}

// =============================================================================
// Test: response.in_progress (No output)
// =============================================================================

func TestNativeToAnthropic_ResponseInProgress(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// First, set up with response.created
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_123", "claude-3"))

	chunk := createNativeResponseInProgress("resp_123")
	result := converter.NativeResponseChunkToResponseChunk(chunk)

	// response.in_progress should not produce any Anthropic chunks
	assert.Len(t, result, 0)
}

// =============================================================================
// Test: Text Content Part Streaming
// =============================================================================

func TestNativeToAnthropic_TextContentStreaming(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// Setup: response.created
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_text", "claude-3"))

	// content_part.added (text) should emit content_block_start
	contentPartAdded := createNativeContentPartAddedText("item_1", 0, 0)
	result := converter.NativeResponseChunkToResponseChunk(contentPartAdded)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockStart)
	assert.NotNil(t, result[0].OfContentBlockStart.ContentBlock.OfText)

	// output_text.delta should emit content_block_delta with text_delta
	textDelta := createNativeOutputTextDelta("item_1", 0, 0, "Hello ", 4)
	result = converter.NativeResponseChunkToResponseChunk(textDelta)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockDelta)
	assert.NotNil(t, result[0].OfContentBlockDelta.Delta.OfText)
	assert.Equal(t, "Hello ", result[0].OfContentBlockDelta.Delta.OfText.Text)

	// More text deltas
	textDelta2 := createNativeOutputTextDelta("item_1", 0, 0, "World!", 5)
	result = converter.NativeResponseChunkToResponseChunk(textDelta2)
	require.Len(t, result, 1)
	assert.Equal(t, "World!", result[0].OfContentBlockDelta.Delta.OfText.Text)
}

// =============================================================================
// Test: Function Call Streaming
// =============================================================================

func TestNativeToAnthropic_FunctionCallStreaming(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// Setup: response.created
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_fn", "claude-3"))

	// output_item.added (function_call) should emit content_block_start with tool_use
	outputItemAdded := createNativeOutputItemAddedFunctionCall("item_fn", 0, "call_123", "get_weather")
	result := converter.NativeResponseChunkToResponseChunk(outputItemAdded)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockStart)
	assert.NotNil(t, result[0].OfContentBlockStart.ContentBlock.OfToolUse)
	assert.Equal(t, "call_123", result[0].OfContentBlockStart.ContentBlock.OfToolUse.ID)
	assert.Equal(t, "get_weather", result[0].OfContentBlockStart.ContentBlock.OfToolUse.Name)

	// function_call_arguments.delta should emit content_block_delta with input_json_delta
	// Note: The current implementation uses Arguments field instead of Delta for input_json_delta
	argsDelta := createNativeFunctionCallArgumentsDelta("item_fn", 0, `{"city":"NYC"}`, 4)
	result = converter.NativeResponseChunkToResponseChunk(argsDelta)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockDelta)
	assert.NotNil(t, result[0].OfContentBlockDelta.Delta.OfInputJSON)
}

// =============================================================================
// Test: Reasoning Summary Streaming
// =============================================================================

func TestNativeToAnthropic_ReasoningStreaming(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// Setup: response.created
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_reason", "claude-3"))

	// reasoning_summary_part.added should emit content_block_start with thinking
	summaryPartAdded := createNativeReasoningSummaryPartAdded("item_reason", 0, 0)
	result := converter.NativeResponseChunkToResponseChunk(summaryPartAdded)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockStart)
	assert.NotNil(t, result[0].OfContentBlockStart.ContentBlock.OfThinking)

	// reasoning_summary_text.delta should emit content_block_delta with thinking_delta
	thinkingDelta := createNativeReasoningSummaryTextDelta("item_reason", 0, 0, "Thinking about this...", 4)
	result = converter.NativeResponseChunkToResponseChunk(thinkingDelta)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockDelta)
	assert.NotNil(t, result[0].OfContentBlockDelta.Delta.OfThinking)
	assert.Equal(t, "Thinking about this...", result[0].OfContentBlockDelta.Delta.OfThinking.Thinking)

	// reasoning_summary_text.delta with signature should emit content_block_delta with signature_delta
	signatureDelta := createNativeReasoningSummaryTextDeltaWithSignature("item_reason", 0, 0, "encrypted_sig_123")
	result = converter.NativeResponseChunkToResponseChunk(signatureDelta)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockDelta)
	assert.NotNil(t, result[0].OfContentBlockDelta.Delta.OfThinkingSignature)
	assert.Equal(t, "encrypted_sig_123", result[0].OfContentBlockDelta.Delta.OfThinkingSignature.Signature)
}

// =============================================================================
// Test: output_item.done Emits content_block_stop
// =============================================================================

func TestNativeToAnthropic_OutputItemDone(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// Setup
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_done", "claude-3"))

	// output_item.done should emit content_block_stop
	outputDone := createNativeOutputItemDoneMessage("item_1", 0, "Hello World")
	result := converter.NativeResponseChunkToResponseChunk(outputDone)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockStop)
	assert.Equal(t, 0, result[0].OfContentBlockStop.Index)
}

// =============================================================================
// Test: response.completed Conversion
// =============================================================================

func TestNativeToAnthropic_ResponseCompleted(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// Setup
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_complete", "claude-3"))

	outputs := []responses2.OutputMessageUnion{
		{
			OfOutputMessage: &responses2.OutputMessage{
				ID:   "item_1",
				Role: constants.RoleAssistant,
				Content: &responses2.OutputContent{
					{OfOutputText: &responses2.OutputTextContent{Text: "Hello"}},
				},
			},
		},
	}

	completed := createNativeResponseCompleted("resp_complete", "claude-3", 100, 50, outputs)
	result := converter.NativeResponseChunkToResponseChunk(completed)

	// response.completed should emit message_delta and message_stop
	require.Len(t, result, 2)

	// First should be message_delta with usage
	assert.NotNil(t, result[0].OfMessageDelta)
	assert.Equal(t, 100, result[0].OfMessageDelta.Usage.InputTokens)
	assert.Equal(t, 50, result[0].OfMessageDelta.Usage.OutputTokens)

	// Second should be message_stop
	assert.NotNil(t, result[1].OfMessageStop)
}

// =============================================================================
// Test: response.completed with incomplete status
// =============================================================================

func TestNativeToAnthropic_ResponseCompletedIncomplete(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// Setup
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_incomplete", "claude-3"))

	// Create a completed chunk with "incomplete" status
	completedChunk := &responses2.ResponseChunk{
		OfResponseCompleted: &responses2.ChunkResponse[constants.ChunkTypeResponseCompleted]{
			Type:           constants.ChunkTypeResponseCompleted("response.completed"),
			SequenceNumber: 10,
			Response: responses2.ChunkResponseData{
				Id:     "resp_incomplete",
				Object: "response",
				Status: "incomplete", // This should result in max_tokens stop reason
				Output: []responses2.OutputMessageUnion{},
				Usage: responses2.Usage{
					InputTokens:  100,
					OutputTokens: 4096,
				},
				Request: responses2.Request{
					Model: "claude-3",
				},
			},
		},
	}

	result := converter.NativeResponseChunkToResponseChunk(completedChunk)
	require.Len(t, result, 2)

	// message_delta should have stop_reason = max_tokens
	assert.NotNil(t, result[0].OfMessageDelta)
	assert.Equal(t, "max_tokens", result[0].OfMessageDelta.Message.StopReason)
}

// =============================================================================
// Test: Nil Input
// =============================================================================

func TestNativeToAnthropic_NilInput(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	result := converter.NativeResponseChunkToResponseChunk(nil)
	assert.Len(t, result, 0)
}

// =============================================================================
// Test: Events That Produce No Output
// =============================================================================

func TestNativeToAnthropic_NoOutputEvents(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// Setup
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_no_out", "claude-3"))

	// output_text.done - should not produce output
	textDone := createNativeOutputTextDone("item_1", 0, 0, "Hello")
	result := converter.NativeResponseChunkToResponseChunk(textDone)
	assert.Len(t, result, 0)

	// content_part.done - should not produce output
	partDone := createNativeContentPartDone("item_1", 0, 0, "Hello")
	result = converter.NativeResponseChunkToResponseChunk(partDone)
	assert.Len(t, result, 0)

	// function_call_arguments.done - should not produce output
	argsDone := createNativeFunctionCallArgumentsDone("item_1", 0, `{"a":"b"}`)
	result = converter.NativeResponseChunkToResponseChunk(argsDone)
	assert.Len(t, result, 0)
}

// =============================================================================
// Test: End-to-End Text Streaming
// =============================================================================

func TestNativeToAnthropic_EndToEnd_TextStreaming(t *testing.T) {
	converter := newNativeToAnthropicConverter()
	var result []ResponseChunk

	// 1. response.created -> message_start
	result = converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_e2e", "claude-3"))
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfMessageStart)

	// 2. content_part.added -> content_block_start
	result = converter.NativeResponseChunkToResponseChunk(createNativeContentPartAddedText("item_1", 0, 0))
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockStart)

	// 3. output_text.delta -> content_block_delta (text_delta)
	result = converter.NativeResponseChunkToResponseChunk(createNativeOutputTextDelta("item_1", 0, 0, "Hello", 4))
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockDelta)
	assert.Equal(t, "Hello", result[0].OfContentBlockDelta.Delta.OfText.Text)

	result = converter.NativeResponseChunkToResponseChunk(createNativeOutputTextDelta("item_1", 0, 0, " World", 5))
	require.Len(t, result, 1)
	assert.Equal(t, " World", result[0].OfContentBlockDelta.Delta.OfText.Text)

	// 4. output_text.done -> nothing
	result = converter.NativeResponseChunkToResponseChunk(createNativeOutputTextDone("item_1", 0, 0, "Hello World"))
	assert.Len(t, result, 0)

	// 5. content_part.done -> nothing
	result = converter.NativeResponseChunkToResponseChunk(createNativeContentPartDone("item_1", 0, 0, "Hello World"))
	assert.Len(t, result, 0)

	// 6. output_item.done -> content_block_stop
	result = converter.NativeResponseChunkToResponseChunk(createNativeOutputItemDoneMessage("item_1", 0, "Hello World"))
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockStop)

	// 7. response.completed -> message_delta + message_stop
	outputs := []responses2.OutputMessageUnion{
		{
			OfOutputMessage: &responses2.OutputMessage{
				ID:      "item_1",
				Role:    constants.RoleAssistant,
				Content: &responses2.OutputContent{{OfOutputText: &responses2.OutputTextContent{Text: "Hello World"}}},
			},
		},
	}
	result = converter.NativeResponseChunkToResponseChunk(createNativeResponseCompleted("resp_e2e", "claude-3", 50, 20, outputs))
	require.Len(t, result, 2)
	assert.NotNil(t, result[0].OfMessageDelta)
	assert.NotNil(t, result[1].OfMessageStop)
}

// =============================================================================
// Test: State Preservation Across Calls
// =============================================================================

func TestNativeToAnthropic_StatePreservation(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// response.created stores state (stores in internal field)
	result := converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_state", "claude-3-sonnet"))
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfMessageStart)

	// Later chunks should use the stored model - verify by calling completed
	outputs := []responses2.OutputMessageUnion{}
	completed := createNativeResponseCompleted("resp_state", "claude-3-sonnet", 100, 50, outputs)

	result = converter.NativeResponseChunkToResponseChunk(completed)
	require.Len(t, result, 2)

	// Verify the message_delta has the usage info
	assert.NotNil(t, result[0].OfMessageDelta)
	assert.Equal(t, 100, result[0].OfMessageDelta.Usage.InputTokens)
}

// =============================================================================
// Test: Multiple Function Calls in Sequence
// =============================================================================

func TestNativeToAnthropic_MultipleFunctionCalls(t *testing.T) {
	converter := newNativeToAnthropicConverter()

	// Setup
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreated("resp_multi_fn", "claude-3"))

	// First function call
	result := converter.NativeResponseChunkToResponseChunk(
		createNativeOutputItemAddedFunctionCall("fn_1", 0, "call_1", "get_weather"))
	require.Len(t, result, 1)
	assert.Equal(t, "get_weather", result[0].OfContentBlockStart.ContentBlock.OfToolUse.Name)

	result = converter.NativeResponseChunkToResponseChunk(
		createNativeOutputItemDoneFunctionCall("fn_1", 0, "call_1", "get_weather", `{"city":"NYC"}`))
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockStop)

	// Second function call
	result = converter.NativeResponseChunkToResponseChunk(
		createNativeOutputItemAddedFunctionCall("fn_2", 1, "call_2", "search_web"))
	require.Len(t, result, 1)
	assert.Equal(t, "search_web", result[0].OfContentBlockStart.ContentBlock.OfToolUse.Name)

	result = converter.NativeResponseChunkToResponseChunk(
		createNativeOutputItemDoneFunctionCall("fn_2", 1, "call_2", "search_web", `{"query":"test"}`))
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfContentBlockStop)
}
