package anthropic_responses

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test Fixtures & Helpers
// =============================================================================

func newConverter() *ResponseChunkToNativeResponseChunkConverter {
	return &ResponseChunkToNativeResponseChunkConverter{}
}

func ptrString(s string) *string {
	return &s
}

// Helper to create a message_start chunk
func createMessageStartChunk(id, model string) *ResponseChunk {
	return &ResponseChunk{
		OfMessageStart: &ChunkMessage[ChunkTypeMessageStart]{
			Type: ChunkTypeMessageStart("message_start"),
			Message: &ChunkMessageData{
				Id:           id,
				Model:        model,
				Type:         "message",
				Role:         RoleAssistant,
				Content:      []interface{}{},
				StopReason:   nil,
				StopSequence: nil,
				Usage: &ChunkMessageUsage{
					InputTokens:  100,
					OutputTokens: 0,
				},
			},
		},
	}
}

// Helper to create a content_block_start for text
func createTextBlockStartChunk(index int) *ResponseChunk {
	return &ResponseChunk{
		OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
			Type:  ChunkTypeContentBlockStart("content_block_start"),
			Index: index,
			ContentBlock: &ContentUnion{
				OfText: &TextContent{
					Type: "text",
					Text: "",
				},
			},
		},
	}
}

// Helper to create a content_block_start for tool_use
func createToolUseBlockStartChunk(index int, toolId, toolName string) *ResponseChunk {
	return &ResponseChunk{
		OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
			Type:  ChunkTypeContentBlockStart("content_block_start"),
			Index: index,
			ContentBlock: &ContentUnion{
				OfToolUse: &ToolUseContent{
					Type:  "tool_use",
					ID:    toolId,
					Name:  toolName,
					Input: map[string]any{},
				},
			},
		},
	}
}

// Helper to create a content_block_start for thinking
func createThinkingBlockStartChunk(index int, signature string) *ResponseChunk {
	return &ResponseChunk{
		OfContentBlockStart: &ChunkContentBlock[ChunkTypeContentBlockStart]{
			Type:  ChunkTypeContentBlockStart("content_block_start"),
			Index: index,
			ContentBlock: &ContentUnion{
				OfThinking: &ThinkingContent{
					Type:      "thinking",
					Thinking:  "",
					Signature: signature,
				},
			},
		},
	}
}

// Helper to create a text_delta chunk
func createTextDeltaChunk(index int, text string) *ResponseChunk {
	return &ResponseChunk{
		OfContentBlockDelta: &ChunkContentBlock[ChunkTypeContentBlockDelta]{
			Type:  ChunkTypeContentBlockDelta("content_block_delta"),
			Index: index,
			Delta: &ChunkContentBlockDeltaUnion{
				OfText: &DeltaTextContent{
					Type: "text_delta",
					Text: text,
				},
			},
		},
	}
}

// Helper to create an input_json_delta chunk (for tool args)
func createInputJSONDeltaChunk(index int, partialJSON string) *ResponseChunk {
	return &ResponseChunk{
		OfContentBlockDelta: &ChunkContentBlock[ChunkTypeContentBlockDelta]{
			Type:  ChunkTypeContentBlockDelta("content_block_delta"),
			Index: index,
			Delta: &ChunkContentBlockDeltaUnion{
				OfInputJSON: &DeltaInputJSONContent{
					Type:        "input_json_delta",
					PartialJSON: partialJSON,
				},
			},
		},
	}
}

// Helper to create a thinking_delta chunk
func createThinkingDeltaChunk(index int, thinking string) *ResponseChunk {
	return &ResponseChunk{
		OfContentBlockDelta: &ChunkContentBlock[ChunkTypeContentBlockDelta]{
			Type:  ChunkTypeContentBlockDelta("content_block_delta"),
			Index: index,
			Delta: &ChunkContentBlockDeltaUnion{
				OfThinking: &DeltaThinkingContent{
					Type:     "thinking_delta",
					Thinking: thinking,
				},
			},
		},
	}
}

// Helper to create a signature_delta chunk
func createSignatureDeltaChunk(index int, signature string) *ResponseChunk {
	return &ResponseChunk{
		OfContentBlockDelta: &ChunkContentBlock[ChunkTypeContentBlockDelta]{
			Type:  ChunkTypeContentBlockDelta("content_block_delta"),
			Index: index,
			Delta: &ChunkContentBlockDeltaUnion{
				OfThinkingSignature: &DeltaThinkingSignatureContent{
					Type:      "signature_delta",
					Signature: signature,
				},
			},
		},
	}
}

// Helper to create a content_block_stop chunk
func createBlockStopChunk(index int) *ResponseChunk {
	return &ResponseChunk{
		OfContentBlockStop: &ChunkContentBlock[ChunkTypeContentBlockStop]{
			Type:  ChunkTypeContentBlockStop("content_block_stop"),
			Index: index,
		},
	}
}

// Helper to create a message_delta chunk
func createMessageDeltaChunk(inputTokens, outputTokens int, stopReason string) *ResponseChunk {
	return &ResponseChunk{
		OfMessageDelta: &ChunkMessage[ChunkTypeMessageDelta]{
			Type: ChunkTypeMessageDelta("message_delta"),
			Usage: &ChunkMessageUsage{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
			Delta: &struct {
				StopReason   interface{} `json:"stop_reason"`
				StopSequence interface{} `json:"stop_sequence"`
			}{
				StopReason:   stopReason,
				StopSequence: nil,
			},
		},
	}
}

// Helper to create a message_stop chunk
func createMessageStopChunk() *ResponseChunk {
	return &ResponseChunk{
		OfMessageStop: &ChunkMessage[ChunkTypeMessageStop]{
			Type: ChunkTypeMessageStop("message_stop"),
		},
	}
}

// Helper to create a ping chunk
func createPingChunk() *ResponseChunk {
	return &ResponseChunk{
		OfPing: &ChunkPing{
			Type: "ping",
		},
	}
}

// =============================================================================
// Test: Message Start Conversion
// =============================================================================

func TestResponseChunkToNative_MessageStart(t *testing.T) {
	converter := newConverter()

	chunk := createMessageStartChunk("msg_123", "claude-3-opus-20240229")
	result := converter.ResponseChunkToNativeResponseChunk(chunk)

	require.Len(t, result, 2, "message_start should emit 2 native chunks")

	// First chunk should be response.created
	assert.NotNil(t, result[0].OfResponseCreated)
	assert.Equal(t, "msg_123", result[0].OfResponseCreated.Response.Id)
	assert.Equal(t, "response", result[0].OfResponseCreated.Response.Object)
	assert.Equal(t, "in_progress", result[0].OfResponseCreated.Response.Status)
	assert.Equal(t, 0, result[0].OfResponseCreated.SequenceNumber)

	// Second chunk should be response.in_progress
	assert.NotNil(t, result[1].OfResponseInProgress)
	assert.Equal(t, "msg_123", result[1].OfResponseInProgress.Response.Id)
	assert.Equal(t, "in_progress", result[1].OfResponseInProgress.Response.Status)
	assert.Equal(t, 1, result[1].OfResponseInProgress.SequenceNumber)
}

// =============================================================================
// Test: Text Content Block Streaming
// =============================================================================

func TestResponseChunkToNative_TextBlockStreaming(t *testing.T) {
	converter := newConverter()

	// Step 1: message_start
	msgStart := createMessageStartChunk("msg_text_123", "claude-3-opus-20240229")
	result := converter.ResponseChunkToNativeResponseChunk(msgStart)
	require.Len(t, result, 2)

	// Step 2: content_block_start (text)
	blockStart := createTextBlockStartChunk(0)
	result = converter.ResponseChunkToNativeResponseChunk(blockStart)
	require.Len(t, result, 2)

	// Should emit output_item.added and content_part.added
	assert.NotNil(t, result[0].OfOutputItemAdded)
	assert.Equal(t, "message", result[0].OfOutputItemAdded.Item.Type)
	assert.Equal(t, constants.RoleAssistant, result[0].OfOutputItemAdded.Item.Role)

	assert.NotNil(t, result[1].OfContentPartAdded)
	assert.NotNil(t, result[1].OfContentPartAdded.Part.OfOutputText)

	// Step 3: text_delta chunks
	deltas := []string{"Hello", ", ", "world", "!"}
	for _, delta := range deltas {
		textDelta := createTextDeltaChunk(0, delta)
		result = converter.ResponseChunkToNativeResponseChunk(textDelta)
		require.Len(t, result, 1)
		assert.NotNil(t, result[0].OfOutputTextDelta)
		assert.Equal(t, delta, result[0].OfOutputTextDelta.Delta)
	}

	// Step 4: content_block_stop
	blockStop := createBlockStopChunk(0)
	result = converter.ResponseChunkToNativeResponseChunk(blockStop)
	require.Len(t, result, 3)

	// Should emit output_text.done, content_part.done, output_item.done
	assert.NotNil(t, result[0].OfOutputTextDone)
	assert.Equal(t, "Hello, world!", *result[0].OfOutputTextDone.Text)

	assert.NotNil(t, result[1].OfContentPartDone)
	assert.Equal(t, "Hello, world!", result[1].OfContentPartDone.Part.OfOutputText.Text)

	assert.NotNil(t, result[2].OfOutputItemDone)
	assert.Equal(t, "message", result[2].OfOutputItemDone.Item.Type)
	assert.Equal(t, "completed", result[2].OfOutputItemDone.Item.Status)
}

// =============================================================================
// Test: Tool Use / Function Call Streaming
// =============================================================================

func TestResponseChunkToNative_ToolUseStreaming(t *testing.T) {
	converter := newConverter()

	// Step 1: message_start
	msgStart := createMessageStartChunk("msg_tool_123", "claude-3-opus-20240229")
	converter.ResponseChunkToNativeResponseChunk(msgStart)

	// Step 2: content_block_start (tool_use)
	blockStart := createToolUseBlockStartChunk(0, "call_abc123", "get_weather")
	result := converter.ResponseChunkToNativeResponseChunk(blockStart)
	require.Len(t, result, 1)

	// Should emit output_item.added for function_call
	assert.NotNil(t, result[0].OfOutputItemAdded)
	assert.Equal(t, "function_call", result[0].OfOutputItemAdded.Item.Type)
	assert.Equal(t, "call_abc123", *result[0].OfOutputItemAdded.Item.CallID)
	assert.Equal(t, "get_weather", *result[0].OfOutputItemAdded.Item.Name)

	// Step 3: input_json_delta chunks (building JSON arguments)
	jsonDeltas := []string{`{"`, `city`, `":"`, `San Francisco`, `","`, `unit`, `":"`, `celsius`, `"}`}
	for _, delta := range jsonDeltas {
		jsonDelta := createInputJSONDeltaChunk(0, delta)
		result = converter.ResponseChunkToNativeResponseChunk(jsonDelta)
		require.Len(t, result, 1)
		assert.NotNil(t, result[0].OfFunctionCallArgumentsDelta)
		assert.Equal(t, delta, result[0].OfFunctionCallArgumentsDelta.Delta)
	}

	// Step 4: content_block_stop
	blockStop := createBlockStopChunk(0)
	result = converter.ResponseChunkToNativeResponseChunk(blockStop)
	require.Len(t, result, 2)

	// Should emit function_call_arguments.done and output_item.done
	assert.NotNil(t, result[0].OfFunctionCallArgumentsDone)
	assert.Equal(t, `{"city":"San Francisco","unit":"celsius"}`, result[0].OfFunctionCallArgumentsDone.Arguments)

	assert.NotNil(t, result[1].OfOutputItemDone)
	assert.Equal(t, "function_call", result[1].OfOutputItemDone.Item.Type)
	assert.Equal(t, "completed", result[1].OfOutputItemDone.Item.Status)
}

// =============================================================================
// Test: Thinking / Reasoning Streaming
// =============================================================================

func TestResponseChunkToNative_ThinkingStreaming(t *testing.T) {
	converter := newConverter()

	// Step 1: message_start
	msgStart := createMessageStartChunk("msg_thinking_123", "claude-3-opus-20240229")
	converter.ResponseChunkToNativeResponseChunk(msgStart)

	// Step 2: content_block_start (thinking)
	blockStart := createThinkingBlockStartChunk(0, "initial_signature_abc")
	result := converter.ResponseChunkToNativeResponseChunk(blockStart)
	require.Len(t, result, 2)

	// Should emit output_item.added for reasoning and reasoning_summary_part.added
	assert.NotNil(t, result[0].OfOutputItemAdded)
	assert.Equal(t, "reasoning", result[0].OfOutputItemAdded.Item.Type)
	assert.NotNil(t, result[0].OfOutputItemAdded.Item.EncryptedContent)

	assert.NotNil(t, result[1].OfReasoningSummaryPartAdded)

	// Step 3: thinking_delta chunks
	thinkingDeltas := []string{"Let me ", "think about ", "this problem..."}
	for _, delta := range thinkingDeltas {
		thinkingDelta := createThinkingDeltaChunk(0, delta)
		result = converter.ResponseChunkToNativeResponseChunk(thinkingDelta)
		require.Len(t, result, 1)
		assert.NotNil(t, result[0].OfReasoningSummaryTextDelta)
		assert.Equal(t, delta, result[0].OfReasoningSummaryTextDelta.Delta)
		assert.Nil(t, result[0].OfReasoningSummaryTextDelta.EncryptedContent)
	}

	// Step 4: signature_delta chunk
	signatureDelta := createSignatureDeltaChunk(0, "final_signature_xyz")
	result = converter.ResponseChunkToNativeResponseChunk(signatureDelta)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfReasoningSummaryTextDelta)
	assert.NotNil(t, result[0].OfReasoningSummaryTextDelta.EncryptedContent)
	assert.Equal(t, "final_signature_xyz", *result[0].OfReasoningSummaryTextDelta.EncryptedContent)

	// Step 5: content_block_stop
	blockStop := createBlockStopChunk(0)
	result = converter.ResponseChunkToNativeResponseChunk(blockStop)
	require.Len(t, result, 3)

	// Should emit reasoning_summary_text.done, reasoning_summary_part.done, output_item.done
	assert.NotNil(t, result[0].OfReasoningSummaryTextDone)
	assert.Equal(t, "Let me think about this problem...", *result[0].OfReasoningSummaryTextDone.Text)

	assert.NotNil(t, result[1].OfReasoningSummaryPartDone)
	assert.Equal(t, "Let me think about this problem...", result[1].OfReasoningSummaryPartDone.Part.Text)

	assert.NotNil(t, result[2].OfOutputItemDone)
	assert.Equal(t, "reasoning", result[2].OfOutputItemDone.Item.Type)
	assert.Equal(t, "completed", result[2].OfOutputItemDone.Item.Status)
	assert.Equal(t, "final_signature_xyz", *result[2].OfOutputItemDone.Item.EncryptedContent)
}

// =============================================================================
// Test: Message Complete
// =============================================================================

func TestResponseChunkToNative_MessageComplete(t *testing.T) {
	converter := newConverter()

	// Setup: message_start and text content
	msgStart := createMessageStartChunk("msg_complete_123", "claude-3-opus-20240229")
	converter.ResponseChunkToNativeResponseChunk(msgStart)

	blockStart := createTextBlockStartChunk(0)
	converter.ResponseChunkToNativeResponseChunk(blockStart)

	textDelta := createTextDeltaChunk(0, "Hello!")
	converter.ResponseChunkToNativeResponseChunk(textDelta)

	blockStop := createBlockStopChunk(0)
	converter.ResponseChunkToNativeResponseChunk(blockStop)

	// Step: message_delta
	msgDelta := createMessageDeltaChunk(100, 50, "end_turn")
	result := converter.ResponseChunkToNativeResponseChunk(msgDelta)
	require.Len(t, result, 0) // message_delta just stores state

	// Step: message_stop
	msgStop := createMessageStopChunk()
	result = converter.ResponseChunkToNativeResponseChunk(msgStop)
	require.Len(t, result, 1)

	// Should emit response.completed
	assert.NotNil(t, result[0].OfResponseCompleted)
	assert.Equal(t, "msg_complete_123", result[0].OfResponseCompleted.Response.Id)
	assert.Equal(t, "completed", result[0].OfResponseCompleted.Response.Status)
	assert.Equal(t, 100, result[0].OfResponseCompleted.Response.Usage.InputTokens)
	assert.Equal(t, 50, result[0].OfResponseCompleted.Response.Usage.OutputTokens)
	assert.Len(t, result[0].OfResponseCompleted.Response.Output, 1)
}

// =============================================================================
// Test: Ping Chunk (Should be ignored)
// =============================================================================

func TestResponseChunkToNative_PingIgnored(t *testing.T) {
	converter := newConverter()

	pingChunk := createPingChunk()
	result := converter.ResponseChunkToNativeResponseChunk(pingChunk)
	assert.Len(t, result, 0, "ping chunks should not produce any output")
}

// =============================================================================
// Test: Nil Input
// =============================================================================

func TestResponseChunkToNative_NilInput(t *testing.T) {
	converter := newConverter()

	result := converter.ResponseChunkToNativeResponseChunk(nil)
	assert.Len(t, result, 0, "nil input should return empty slice")
}

// =============================================================================
// Test: Multiple Content Blocks (Text + Tool Call)
// =============================================================================

func TestResponseChunkToNative_MultipleContentBlocks(t *testing.T) {
	converter := newConverter()

	// message_start
	msgStart := createMessageStartChunk("msg_multi_123", "claude-3-opus-20240229")
	converter.ResponseChunkToNativeResponseChunk(msgStart)

	// First block: text
	textBlockStart := createTextBlockStartChunk(0)
	converter.ResponseChunkToNativeResponseChunk(textBlockStart)

	textDelta := createTextDeltaChunk(0, "I'll check the weather for you.")
	converter.ResponseChunkToNativeResponseChunk(textDelta)

	textBlockStop := createBlockStopChunk(0)
	result := converter.ResponseChunkToNativeResponseChunk(textBlockStop)
	require.Len(t, result, 3)
	assert.Equal(t, "I'll check the weather for you.", *result[0].OfOutputTextDone.Text)

	// Second block: tool_use
	toolBlockStart := createToolUseBlockStartChunk(1, "call_weather_123", "get_weather")
	result = converter.ResponseChunkToNativeResponseChunk(toolBlockStart)
	require.Len(t, result, 1)
	assert.Equal(t, "function_call", result[0].OfOutputItemAdded.Item.Type)

	toolDelta := createInputJSONDeltaChunk(1, `{"city":"NYC"}`)
	converter.ResponseChunkToNativeResponseChunk(toolDelta)

	toolBlockStop := createBlockStopChunk(1)
	result = converter.ResponseChunkToNativeResponseChunk(toolBlockStop)
	require.Len(t, result, 2)
	assert.Equal(t, `{"city":"NYC"}`, result[0].OfFunctionCallArgumentsDone.Arguments)

	// Complete message
	msgDelta := createMessageDeltaChunk(100, 75, "end_turn")
	converter.ResponseChunkToNativeResponseChunk(msgDelta)

	msgStop := createMessageStopChunk()
	result = converter.ResponseChunkToNativeResponseChunk(msgStop)
	require.Len(t, result, 1)

	// Verify both outputs are in the completed response
	assert.Len(t, result[0].OfResponseCompleted.Response.Output, 2)
}

// =============================================================================
// Test: Sequence Number Incrementing
// =============================================================================

func TestResponseChunkToNative_SequenceNumberIncrement(t *testing.T) {
	converter := newConverter()

	// message_start produces 2 chunks
	msgStart := createMessageStartChunk("msg_seq_123", "claude-3-opus-20240229")
	result := converter.ResponseChunkToNativeResponseChunk(msgStart)
	assert.Equal(t, 0, result[0].OfResponseCreated.SequenceNumber)
	assert.Equal(t, 1, result[1].OfResponseInProgress.SequenceNumber)

	// text block start produces 2 chunks
	blockStart := createTextBlockStartChunk(0)
	result = converter.ResponseChunkToNativeResponseChunk(blockStart)
	assert.Equal(t, 2, result[0].OfOutputItemAdded.SequenceNumber)
	assert.Equal(t, 3, result[1].OfContentPartAdded.SequenceNumber)

	// text delta produces 1 chunk
	textDelta := createTextDeltaChunk(0, "Hi")
	result = converter.ResponseChunkToNativeResponseChunk(textDelta)
	assert.Equal(t, 4, result[0].OfOutputTextDelta.SequenceNumber)
}

// =============================================================================
// Test: End-to-End Complex Scenario (Reasoning + Text + Function Call)
// =============================================================================

func TestResponseChunkToNative_EndToEnd_ReasoningTextFunctionCall(t *testing.T) {
	converter := newConverter()
	var result []*responses.ResponseChunk

	// 1. message_start
	result = converter.ResponseChunkToNativeResponseChunk(createMessageStartChunk("msg_e2e_123", "claude-3-opus-20240229"))
	require.Len(t, result, 2)

	// 2. Reasoning block
	result = converter.ResponseChunkToNativeResponseChunk(createThinkingBlockStartChunk(0, "sig_init"))
	require.Len(t, result, 2)

	result = converter.ResponseChunkToNativeResponseChunk(createThinkingDeltaChunk(0, "Analyzing the request..."))
	require.Len(t, result, 1)

	result = converter.ResponseChunkToNativeResponseChunk(createSignatureDeltaChunk(0, "sig_final_reasoning"))
	require.Len(t, result, 1)

	result = converter.ResponseChunkToNativeResponseChunk(createBlockStopChunk(0))
	require.Len(t, result, 3)

	// 3. Text block
	result = converter.ResponseChunkToNativeResponseChunk(createTextBlockStartChunk(1))
	require.Len(t, result, 2)

	result = converter.ResponseChunkToNativeResponseChunk(createTextDeltaChunk(1, "I'll help you with that."))
	require.Len(t, result, 1)

	result = converter.ResponseChunkToNativeResponseChunk(createBlockStopChunk(1))
	require.Len(t, result, 3)

	// 4. Function call block
	result = converter.ResponseChunkToNativeResponseChunk(createToolUseBlockStartChunk(2, "call_func_123", "search_web"))
	require.Len(t, result, 1)

	result = converter.ResponseChunkToNativeResponseChunk(createInputJSONDeltaChunk(2, `{"query":"test"}`))
	require.Len(t, result, 1)

	result = converter.ResponseChunkToNativeResponseChunk(createBlockStopChunk(2))
	require.Len(t, result, 2)

	// 5. Complete message
	result = converter.ResponseChunkToNativeResponseChunk(createMessageDeltaChunk(200, 150, "tool_use"))
	require.Len(t, result, 0)

	result = converter.ResponseChunkToNativeResponseChunk(createMessageStopChunk())
	require.Len(t, result, 1)

	// Verify final response
	assert.NotNil(t, result[0].OfResponseCompleted)
	assert.Equal(t, "completed", result[0].OfResponseCompleted.Response.Status)
	assert.Len(t, result[0].OfResponseCompleted.Response.Output, 3)

	// Verify output types in order
	outputs := result[0].OfResponseCompleted.Response.Output
	assert.NotNil(t, outputs[0].OfReasoning, "First output should be reasoning")
	assert.NotNil(t, outputs[1].OfOutputMessage, "Second output should be message")
	assert.NotNil(t, outputs[2].OfFunctionCall, "Third output should be function call")
}

// =============================================================================
// Test: Empty Tool Arguments
// =============================================================================

func TestResponseChunkToNative_EmptyToolArguments(t *testing.T) {
	converter := newConverter()

	msgStart := createMessageStartChunk("msg_empty_args", "claude-3-opus-20240229")
	converter.ResponseChunkToNativeResponseChunk(msgStart)

	blockStart := createToolUseBlockStartChunk(0, "call_empty", "no_params_tool")
	converter.ResponseChunkToNativeResponseChunk(blockStart)

	// No input_json_delta - directly to content_block_stop
	blockStop := createBlockStopChunk(0)
	result := converter.ResponseChunkToNativeResponseChunk(blockStop)

	require.Len(t, result, 2)
	assert.NotNil(t, result[0].OfFunctionCallArgumentsDone)
	assert.Equal(t, "{}", result[0].OfFunctionCallArgumentsDone.Arguments, "Empty arguments should default to {}")
}

// =============================================================================
// Test: Output Index Tracking
// =============================================================================

func TestResponseChunkToNative_OutputIndexTracking(t *testing.T) {
	converter := newConverter()

	msgStart := createMessageStartChunk("msg_idx", "claude-3-opus-20240229")
	converter.ResponseChunkToNativeResponseChunk(msgStart)

	// First block
	converter.ResponseChunkToNativeResponseChunk(createTextBlockStartChunk(0))
	converter.ResponseChunkToNativeResponseChunk(createTextDeltaChunk(0, "First"))
	result := converter.ResponseChunkToNativeResponseChunk(createBlockStopChunk(0))
	assert.Equal(t, 0, result[2].OfOutputItemDone.OutputIndex)

	// Second block
	converter.ResponseChunkToNativeResponseChunk(createTextBlockStartChunk(1))
	converter.ResponseChunkToNativeResponseChunk(createTextDeltaChunk(1, "Second"))
	result = converter.ResponseChunkToNativeResponseChunk(createBlockStopChunk(1))
	assert.Equal(t, 1, result[2].OfOutputItemDone.OutputIndex) // Second output block increments the output index.
}
