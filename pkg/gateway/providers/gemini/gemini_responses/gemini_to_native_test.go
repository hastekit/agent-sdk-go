package gemini_responses

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test Fixtures & Helpers for Gemini → Native
// =============================================================================

func newGeminiToNativeConverter() *ResponseChunkToNativeResponseChunkConverter {
	return &ResponseChunkToNativeResponseChunkConverter{}
}

// Helper to create a Gemini response chunk with text
func createGeminiTextChunk(responseId, modelVersion, text string, promptTokens, candidateTokens, totalTokens int) *Response {
	return &Response{
		ResponseID:   responseId,
		ModelVersion: modelVersion,
		Candidates: []Candidate{
			{
				Content: Content{
					Role: RoleModel,
					Parts: []Part{
						{Text: utils.Ptr(text)},
					},
				},
				FinishReason: "",
			},
		},
		UsageMetadata: &UsageMetadata{
			PromptTokenCount:     promptTokens,
			CandidatesTokenCount: candidateTokens,
			TotalTokenCount:      totalTokens,
		},
	}
}

// Helper to create a Gemini response chunk with function call
func createGeminiFunctionCallChunk(responseId, modelVersion, fnName string, fnArgs map[string]any, promptTokens, candidateTokens, totalTokens int) *Response {
	return &Response{
		ResponseID:   responseId,
		ModelVersion: modelVersion,
		Candidates: []Candidate{
			{
				Content: Content{
					Role: RoleModel,
					Parts: []Part{
						{
							FunctionCall: &FunctionCall{
								Name: fnName,
								Args: fnArgs,
							},
						},
					},
				},
				FinishReason: "",
			},
		},
		UsageMetadata: &UsageMetadata{
			PromptTokenCount:     promptTokens,
			CandidatesTokenCount: candidateTokens,
			TotalTokenCount:      totalTokens,
		},
	}
}

// Helper to create a Gemini response chunk with finished state
func createGeminiFinishedChunk(responseId, modelVersion, finishReason string, promptTokens, candidateTokens, totalTokens int) *Response {
	return &Response{
		ResponseID:   responseId,
		ModelVersion: modelVersion,
		Candidates: []Candidate{
			{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{},
				},
				FinishReason: finishReason,
			},
		},
		UsageMetadata: &UsageMetadata{
			PromptTokenCount:     promptTokens,
			CandidatesTokenCount: candidateTokens,
			TotalTokenCount:      totalTokens,
		},
	}
}

// =============================================================================
// Test: First Chunk Emits response.created and response.in_progress
// =============================================================================

func TestGeminiToNative_FirstChunkEmitsCreatedAndInProgress(t *testing.T) {
	converter := newGeminiToNativeConverter()

	chunk := createGeminiTextChunk("resp_123", "gemini-1.5-pro", "Hello", 100, 10, 110)
	result := converter.ResponseChunkToNativeResponseChunk(chunk)

	// First chunk should emit: response.created, response.in_progress, output_item.added, content_part.added, output_text.delta
	require.GreaterOrEqual(t, len(result), 4)

	// First should be response.created
	assert.NotNil(t, result[0].OfResponseCreated)
	assert.Equal(t, "resp_123", result[0].OfResponseCreated.Response.Id)
	assert.Equal(t, "in_progress", result[0].OfResponseCreated.Response.Status)

	// Second should be response.in_progress
	assert.NotNil(t, result[1].OfResponseInProgress)
	assert.Equal(t, "in_progress", result[1].OfResponseInProgress.Response.Status)
}

// =============================================================================
// Test: Text Streaming
// =============================================================================

func TestGeminiToNative_TextStreaming(t *testing.T) {
	converter := newGeminiToNativeConverter()

	// First chunk with "Hello"
	chunk1 := createGeminiTextChunk("resp_text", "gemini-1.5-pro", "Hello", 100, 5, 105)
	result := converter.ResponseChunkToNativeResponseChunk(chunk1)

	// Should emit response.created, in_progress, output_item.added, content_part.added, output_text.delta
	require.GreaterOrEqual(t, len(result), 4)

	// Find the output_text.delta
	var foundTextDelta bool
	for _, r := range result {
		if r.OfOutputTextDelta != nil {
			foundTextDelta = true
			assert.Equal(t, "Hello", r.OfOutputTextDelta.Delta)
		}
	}
	assert.True(t, foundTextDelta, "Should have found output_text.delta")

	// Second chunk with " World"
	chunk2 := createGeminiTextChunk("resp_text", "gemini-1.5-pro", " World", 100, 10, 110)
	result = converter.ResponseChunkToNativeResponseChunk(chunk2)

	// Should only emit output_text.delta (no more created/in_progress)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].OfOutputTextDelta)
	assert.Equal(t, " World", result[0].OfOutputTextDelta.Delta)
}

// =============================================================================
// Test: Function Call
// =============================================================================

func TestGeminiToNative_FunctionCall(t *testing.T) {
	converter := newGeminiToNativeConverter()

	args := map[string]any{
		"city": "San Francisco",
		"unit": "celsius",
	}
	chunk := createGeminiFunctionCallChunk("resp_fn", "gemini-1.5-pro", "get_weather", args, 100, 20, 120)
	result := converter.ResponseChunkToNativeResponseChunk(chunk)

	// Should emit response.created, in_progress, output_item.added (function_call), function_call_arguments.delta
	require.GreaterOrEqual(t, len(result), 4)

	// Find output_item.added for function_call
	var foundFunctionCall bool
	for _, r := range result {
		if r.OfOutputItemAdded != nil && r.OfOutputItemAdded.Item.Type == "function_call" {
			foundFunctionCall = true
			assert.Equal(t, "get_weather", *r.OfOutputItemAdded.Item.Name)
		}
	}
	assert.True(t, foundFunctionCall, "Should have found function_call output_item.added")
}

// =============================================================================
// Test: Nil Input (Stream End) Emits response.completed
// =============================================================================

func TestGeminiToNative_NilInputEmitsCompleted(t *testing.T) {
	converter := newGeminiToNativeConverter()

	// First, send some chunks to establish state
	chunk1 := createGeminiTextChunk("resp_nil", "gemini-1.5-pro", "Hello", 100, 10, 110)
	converter.ResponseChunkToNativeResponseChunk(chunk1)

	// Nil input signals end of stream
	result := converter.ResponseChunkToNativeResponseChunk(nil)

	// Should emit text completion events and response.completed
	var foundCompleted bool
	for _, r := range result {
		if r.OfResponseCompleted != nil {
			foundCompleted = true
			assert.Equal(t, "completed", r.OfResponseCompleted.Response.Status)
			assert.Equal(t, "resp_nil", r.OfResponseCompleted.Response.Id)
		}
	}
	assert.True(t, foundCompleted, "Should have found response.completed")
}

// =============================================================================
// Test: Stream Already Ended Returns Empty
// =============================================================================

func TestGeminiToNative_StreamEndedReturnsEmpty(t *testing.T) {
	converter := newGeminiToNativeConverter()

	// Send chunks and end stream
	chunk := createGeminiTextChunk("resp_ended", "gemini-1.5-pro", "Test", 100, 5, 105)
	converter.ResponseChunkToNativeResponseChunk(chunk)
	converter.ResponseChunkToNativeResponseChunk(nil) // End stream

	// Subsequent nil should return empty
	result := converter.ResponseChunkToNativeResponseChunk(nil)
	assert.Len(t, result, 0)

	// Subsequent chunks should also return empty
	chunk2 := createGeminiTextChunk("resp_ended", "gemini-1.5-pro", "More", 100, 10, 110)
	result = converter.ResponseChunkToNativeResponseChunk(chunk2)
	assert.Len(t, result, 0)
}

// =============================================================================
// Test: Sequence Number Incrementing
// =============================================================================

func TestGeminiToNative_SequenceNumberIncrement(t *testing.T) {
	converter := newGeminiToNativeConverter()

	chunk := createGeminiTextChunk("resp_seq", "gemini-1.5-pro", "Test", 100, 5, 105)
	result := converter.ResponseChunkToNativeResponseChunk(chunk)

	// Verify sequence numbers are incrementing
	seqNums := []int{}
	for _, r := range result {
		if r.OfResponseCreated != nil {
			seqNums = append(seqNums, r.OfResponseCreated.SequenceNumber)
		}
		if r.OfResponseInProgress != nil {
			seqNums = append(seqNums, r.OfResponseInProgress.SequenceNumber)
		}
		if r.OfOutputItemAdded != nil {
			seqNums = append(seqNums, r.OfOutputItemAdded.SequenceNumber)
		}
	}

	// Should be monotonically increasing
	for i := 1; i < len(seqNums); i++ {
		assert.Greater(t, seqNums[i], seqNums[i-1], "Sequence numbers should be monotonically increasing")
	}
}

// =============================================================================
// Test: Usage Metadata in Completed Response
// =============================================================================

func TestGeminiToNative_UsageInCompletedResponse(t *testing.T) {
	converter := newGeminiToNativeConverter()

	chunk := createGeminiTextChunk("resp_usage", "gemini-1.5-pro", "Test", 150, 75, 225)
	converter.ResponseChunkToNativeResponseChunk(chunk)

	// End stream to get completed response
	result := converter.ResponseChunkToNativeResponseChunk(nil)

	// Find response.completed and verify usage
	var completed *responses.ChunkResponse[constants.ChunkTypeResponseCompleted]
	for _, r := range result {
		if r.OfResponseCompleted != nil {
			completed = r.OfResponseCompleted
			break
		}
	}

	require.NotNil(t, completed)
	assert.Equal(t, 150, completed.Response.Usage.InputTokens)
	assert.Equal(t, 75, completed.Response.Usage.OutputTokens)
	assert.Equal(t, 225, completed.Response.Usage.TotalTokens)
}

// =============================================================================
// Test: Multiple Text Chunks Accumulate
// =============================================================================

func TestGeminiToNative_MultipleTextChunksAccumulate(t *testing.T) {
	converter := newGeminiToNativeConverter()

	// Send multiple text chunks
	chunks := []string{"Hello", ", ", "how ", "are ", "you?"}

	for i, text := range chunks {
		chunk := createGeminiTextChunk("resp_multi", "gemini-1.5-pro", text, 100, i*5, 100+i*5)
		result := converter.ResponseChunkToNativeResponseChunk(chunk)

		// After first chunk, should only get text delta
		if i > 0 {
			require.Len(t, result, 1)
			assert.NotNil(t, result[0].OfOutputTextDelta)
			assert.Equal(t, text, result[0].OfOutputTextDelta.Delta)
		}
	}

	// End stream
	result := converter.ResponseChunkToNativeResponseChunk(nil)

	// Find the output_text.done to verify accumulated text
	var foundTextDone bool
	for _, r := range result {
		if r.OfOutputTextDone != nil {
			foundTextDone = true
			assert.Equal(t, "Hello, how are you?", *r.OfOutputTextDone.Text)
		}
	}
	assert.True(t, foundTextDone, "Should have found output_text.done with accumulated text")
}

// =============================================================================
// Test: Content Type Transition (Text to Function)
// =============================================================================

func TestGeminiToNative_ContentTypeTransition(t *testing.T) {
	converter := newGeminiToNativeConverter()

	// First chunk with text
	textChunk := createGeminiTextChunk("resp_trans", "gemini-1.5-pro", "Let me help you", 100, 10, 110)
	result := converter.ResponseChunkToNativeResponseChunk(textChunk)
	require.GreaterOrEqual(t, len(result), 4)

	// Second chunk with function call - should end previous text block
	fnArgs := map[string]any{"query": "test"}
	fnChunk := createGeminiFunctionCallChunk("resp_trans", "gemini-1.5-pro", "search", fnArgs, 100, 20, 120)
	result = converter.ResponseChunkToNativeResponseChunk(fnChunk)

	// Should have ended previous text block and started function call
	var foundTextDone, foundFnAdded bool
	for _, r := range result {
		if r.OfOutputTextDone != nil {
			foundTextDone = true
		}
		if r.OfOutputItemAdded != nil && r.OfOutputItemAdded.Item.Type == "function_call" {
			foundFnAdded = true
		}
	}
	assert.True(t, foundTextDone, "Should have ended text block")
	assert.True(t, foundFnAdded, "Should have started function call")
}

// =============================================================================
// Test: Empty Response Parts
// =============================================================================

func TestGeminiToNative_EmptyResponseParts(t *testing.T) {
	converter := newGeminiToNativeConverter()

	// Response with empty parts
	chunk := &Response{
		ResponseID:   "resp_empty",
		ModelVersion: "gemini-1.5-pro",
		Candidates: []Candidate{
			{
				Content: Content{
					Role:  RoleModel,
					Parts: []Part{}, // Empty parts
				},
				FinishReason: "",
			},
		},
		UsageMetadata: &UsageMetadata{
			PromptTokenCount:     100,
			CandidatesTokenCount: 0,
			TotalTokenCount:      100,
		},
	}

	result := converter.ResponseChunkToNativeResponseChunk(chunk)

	// Should still emit created and in_progress but no content deltas
	var foundCreated, foundInProgress bool
	for _, r := range result {
		if r.OfResponseCreated != nil {
			foundCreated = true
		}
		if r.OfResponseInProgress != nil {
			foundInProgress = true
		}
	}
	assert.True(t, foundCreated)
	assert.True(t, foundInProgress)
}

// =============================================================================
// Test: Output Message Union in Completed Response
// =============================================================================

func TestGeminiToNative_OutputMessageUnionInCompleted(t *testing.T) {
	converter := newGeminiToNativeConverter()

	// Send text chunk
	textChunk := createGeminiTextChunk("resp_output", "gemini-1.5-pro", "Hello World", 100, 10, 110)
	converter.ResponseChunkToNativeResponseChunk(textChunk)

	// End stream
	result := converter.ResponseChunkToNativeResponseChunk(nil)

	// Find response.completed and verify output
	var completed *responses.ChunkResponse[constants.ChunkTypeResponseCompleted]
	for _, r := range result {
		if r.OfResponseCompleted != nil {
			completed = r.OfResponseCompleted
			break
		}
	}

	require.NotNil(t, completed)
	require.Len(t, completed.Response.Output, 1)
	assert.NotNil(t, completed.Response.Output[0].OfOutputMessage)
}

// =============================================================================
// Test: Function Call Output in Completed Response
// =============================================================================

func TestGeminiToNative_FunctionCallInCompleted(t *testing.T) {
	converter := newGeminiToNativeConverter()

	args := map[string]any{"city": "NYC"}
	fnChunk := createGeminiFunctionCallChunk("resp_fn_complete", "gemini-1.5-pro", "get_weather", args, 100, 15, 115)
	converter.ResponseChunkToNativeResponseChunk(fnChunk)

	// End stream
	result := converter.ResponseChunkToNativeResponseChunk(nil)

	// Find response.completed and verify function call output
	var completed *responses.ChunkResponse[constants.ChunkTypeResponseCompleted]
	for _, r := range result {
		if r.OfResponseCompleted != nil {
			completed = r.OfResponseCompleted
			break
		}
	}

	require.NotNil(t, completed)
	require.Len(t, completed.Response.Output, 1)
	assert.NotNil(t, completed.Response.Output[0].OfFunctionCall)
	assert.Equal(t, "get_weather", completed.Response.Output[0].OfFunctionCall.Name)
}

// =============================================================================
// Test: Model Version Propagation
// =============================================================================

func TestGeminiToNative_ModelVersionPropagation(t *testing.T) {
	converter := newGeminiToNativeConverter()

	chunk := createGeminiTextChunk("resp_model", "gemini-1.5-flash-002", "Test", 100, 5, 105)
	result := converter.ResponseChunkToNativeResponseChunk(chunk)

	// Find response.created and verify model
	for _, r := range result {
		if r.OfResponseCreated != nil {
			assert.Equal(t, "gemini-1.5-flash-002", r.OfResponseCreated.Response.Request.Model)
			return
		}
	}
	t.Fatal("Should have found response.created")
}
