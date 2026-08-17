package gemini_responses

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test Fixtures & Helpers for Native → Gemini
// =============================================================================

func newNativeToGeminiConverter() *NativeResponseChunkToResponseChunkConverter {
	return &NativeResponseChunkToResponseChunkConverter{}
}

// Helper to create a native response.created chunk for Gemini tests
func createNativeResponseCreatedForGemini(id, model string) *responses2.ResponseChunk {
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

// Helper to create a native output_text.delta for Gemini tests
func createNativeOutputTextDeltaForGemini(itemId string, outputIndex, contentIndex int, delta string, seqNum int) *responses2.ResponseChunk {
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

// Helper to create a native output_item.added for function_call for Gemini tests
func createNativeOutputItemAddedFunctionCallForGemini(itemId string, outputIndex int, callId, name string, args any) *responses2.ResponseChunk {
	argsStr := ""
	if args != nil {
		argsStr = args.(string)
	}
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
				Arguments: utils.Ptr(argsStr),
			},
		},
	}
}

// =============================================================================
// Test: response.created Stores State
// =============================================================================

func TestNativeToGemini_ResponseCreatedStoresState(t *testing.T) {
	converter := newNativeToGeminiConverter()

	chunk := createNativeResponseCreatedForGemini("resp_123", "gemini-1.5-pro")
	result := converter.NativeResponseChunkToResponseChunk(chunk)

	// response.created should not produce Gemini output (Gemini doesn't have this event)
	// but it should store state for later use
	assert.Len(t, result, 0)
}

// =============================================================================
// Test: output_text.delta Emits Gemini Text Part
// =============================================================================

func TestNativeToGemini_OutputTextDelta(t *testing.T) {
	converter := newNativeToGeminiConverter()

	// First, set up with response.created
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreatedForGemini("resp_text", "gemini-1.5-pro"))

	// Text delta should emit Gemini response with text part
	textDelta := createNativeOutputTextDeltaForGemini("item_1", 0, 0, "Hello World", 3)
	result := converter.NativeResponseChunkToResponseChunk(textDelta)

	require.Len(t, result, 1)
	require.Len(t, result[0].Candidates, 1)
	require.Len(t, result[0].Candidates[0].Content.Parts, 1)
	assert.NotNil(t, result[0].Candidates[0].Content.Parts[0].Text)
	assert.Equal(t, "Hello World", *result[0].Candidates[0].Content.Parts[0].Text)
	assert.Equal(t, RoleModel, result[0].Candidates[0].Content.Role)
}

// =============================================================================
// Test: Function Call Emits Gemini FunctionCall Part
// =============================================================================

func TestNativeToGemini_FunctionCall(t *testing.T) {
	converter := newNativeToGeminiConverter()

	// Set up with response.created
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreatedForGemini("resp_fn", "gemini-1.5-pro"))

	// output_item.added with function_call should emit Gemini response with function_call part
	fnChunk := createNativeOutputItemAddedFunctionCallForGemini("fn_1", 0, "call_123", "get_weather", `{"city":"NYC"}`)
	result := converter.NativeResponseChunkToResponseChunk(fnChunk)

	require.Len(t, result, 1)
	require.Len(t, result[0].Candidates, 1)
	require.Len(t, result[0].Candidates[0].Content.Parts, 1)
	assert.NotNil(t, result[0].Candidates[0].Content.Parts[0].FunctionCall)
	assert.Equal(t, "get_weather", result[0].Candidates[0].Content.Parts[0].FunctionCall.Name)
}

// =============================================================================
// Test: Nil Input Returns Empty
// =============================================================================

func TestNativeToGemini_NilInput(t *testing.T) {
	converter := newNativeToGeminiConverter()

	result := converter.NativeResponseChunkToResponseChunk(nil)
	assert.Len(t, result, 0)
}

// =============================================================================
// Test: Model and ResponseID Propagation
// =============================================================================

func TestNativeToGemini_ModelAndIdPropagation(t *testing.T) {
	converter := newNativeToGeminiConverter()

	// Set up with response.created
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreatedForGemini("resp_model_test", "gemini-1.5-flash"))

	// Text delta should use the stored model and response ID
	textDelta := createNativeOutputTextDeltaForGemini("item_1", 0, 0, "Test", 3)
	result := converter.NativeResponseChunkToResponseChunk(textDelta)

	require.Len(t, result, 1)
	assert.Equal(t, "gemini-1.5-flash", result[0].ModelVersion)
	assert.Equal(t, "resp_model_test", result[0].ResponseID)
}

// =============================================================================
// Test: Multiple Text Deltas
// =============================================================================

func TestNativeToGemini_MultipleTextDeltas(t *testing.T) {
	converter := newNativeToGeminiConverter()

	// Set up
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreatedForGemini("resp_multi", "gemini-1.5-pro"))

	// Multiple text deltas
	deltas := []string{"Hello", ", ", "World", "!"}
	for i, delta := range deltas {
		textDelta := createNativeOutputTextDeltaForGemini("item_1", 0, 0, delta, i+3)
		result := converter.NativeResponseChunkToResponseChunk(textDelta)

		require.Len(t, result, 1)
		assert.Equal(t, delta, *result[0].Candidates[0].Content.Parts[0].Text)
	}
}

// =============================================================================
// Test: Events That Produce No Output
// =============================================================================

func TestNativeToGemini_NoOutputEvents(t *testing.T) {
	converter := newNativeToGeminiConverter()

	// Set up
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreatedForGemini("resp_no_out", "gemini-1.5-pro"))

	// response.in_progress - should not produce output
	inProgress := &responses2.ResponseChunk{
		OfResponseInProgress: &responses2.ChunkResponse[constants.ChunkTypeResponseInProgress]{
			Type:           constants.ChunkTypeResponseInProgress("response.in_progress"),
			SequenceNumber: 1,
			Response:       responses2.ChunkResponseData{Id: "resp_no_out"},
		},
	}
	result := converter.NativeResponseChunkToResponseChunk(inProgress)
	assert.Len(t, result, 0)

	// output_text.done - should not produce output (Gemini doesn't distinguish done events)
	textDone := &responses2.ResponseChunk{
		OfOutputTextDone: &responses2.ChunkOutputText[constants.ChunkTypeOutputTextDone]{
			Type:           constants.ChunkTypeOutputTextDone("response.output_text.done"),
			SequenceNumber: 5,
			ItemId:         "item_1",
			Text:           utils.Ptr("Final text"),
		},
	}
	result = converter.NativeResponseChunkToResponseChunk(textDone)
	assert.Len(t, result, 0)

	// content_part.added - should not produce output for Gemini
	contentPartAdded := &responses2.ResponseChunk{
		OfContentPartAdded: &responses2.ChunkContentPart[constants.ChunkTypeContentPartAdded]{
			Type:           constants.ChunkTypeContentPartAdded("response.content_part.added"),
			SequenceNumber: 3,
			ItemId:         "item_1",
			Part: responses2.ChunkOutputItemContentUnion{
				OfOutputText: &responses2.OutputTextContent{Text: ""},
			},
		},
	}
	result = converter.NativeResponseChunkToResponseChunk(contentPartAdded)
	assert.Len(t, result, 0)
}

// =============================================================================
// Test: output_item.added with message type (Not function_call)
// =============================================================================

func TestNativeToGemini_OutputItemAddedMessage(t *testing.T) {
	converter := newNativeToGeminiConverter()

	// Set up
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreatedForGemini("resp_msg", "gemini-1.5-pro"))

	// output_item.added with type "message" should not produce output (text comes from deltas)
	msgAdded := &responses2.ResponseChunk{
		OfOutputItemAdded: &responses2.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{
			Type:           constants.ChunkTypeOutputItemAdded("response.output_item.added"),
			SequenceNumber: 2,
			OutputIndex:    0,
			Item: responses2.ChunkOutputItemData{
				Type:   "message",
				Id:     "msg_1",
				Status: "in_progress",
				Role:   constants.RoleAssistant,
			},
		},
	}
	result := converter.NativeResponseChunkToResponseChunk(msgAdded)
	assert.Len(t, result, 0)
}

// =============================================================================
// Test: Gemini Response Role
// =============================================================================

func TestNativeToGemini_ResponseRole(t *testing.T) {
	converter := newNativeToGeminiConverter()

	// Set up
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreatedForGemini("resp_role", "gemini-1.5-pro"))

	textDelta := createNativeOutputTextDeltaForGemini("item_1", 0, 0, "Test", 3)
	result := converter.NativeResponseChunkToResponseChunk(textDelta)

	require.Len(t, result, 1)
	assert.Equal(t, RoleModel, result[0].Candidates[0].Content.Role)
}

// =============================================================================
// Test: Parts Only When Content Exists
// =============================================================================

func TestNativeToGemini_PartsOnlyWithContent(t *testing.T) {
	converter := newNativeToGeminiConverter()

	// Set up
	converter.NativeResponseChunkToResponseChunk(createNativeResponseCreatedForGemini("resp_parts", "gemini-1.5-pro"))

	// Function call delta - should produce response with parts
	fnDelta := &responses2.ResponseChunk{
		OfFunctionCallArgumentsDelta: &responses2.ChunkFunctionCall[constants.ChunkTypeFunctionCallArgumentsDelta]{
			Type:           constants.ChunkTypeFunctionCallArgumentsDelta("response.function_call_arguments.delta"),
			SequenceNumber: 4,
			ItemId:         "fn_1",
			OutputIndex:    0,
			Delta:          `{"city":"NYC"}`,
		},
	}
	result := converter.NativeResponseChunkToResponseChunk(fnDelta)

	// function_call_arguments.delta doesn't produce Gemini output
	// (Gemini sends complete function calls in one chunk)
	assert.Len(t, result, 0)
}
