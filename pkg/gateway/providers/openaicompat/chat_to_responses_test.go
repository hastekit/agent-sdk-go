package openaicompat

import (
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

func TestChatResponseToNativeResponse(t *testing.T) {
	body := `{
		"id": "chatcmpl-1",
		"model": "deepseek-reasoner",
		"choices": [{
			"index": 0,
			"finish_reason": "tool_calls",
			"message": {
				"role": "assistant",
				"content": "let me look that up",
				"reasoning_content": "the user wants weather",
				"tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "weather", "arguments": "{\"city\":\"pune\"}"}}
				]
			}
		}],
		"usage": {"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150,
			"prompt_tokens_details": {"cached_tokens": 100},
			"completion_tokens_details": {"reasoning_tokens": 12}}
	}`

	var chatResponse ChatResponse
	if err := sonic.Unmarshal([]byte(body), &chatResponse); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	out := chatResponse.ToNativeResponse()

	if len(out.Output) != 3 {
		t.Fatalf("output = %d items, want reasoning + message + function call", len(out.Output))
	}

	if out.Output[0].OfReasoning == nil || out.Output[0].OfReasoning.Summary[0].Text != "the user wants weather" {
		t.Errorf("output[0] = %+v, want the reasoning item first", out.Output[0])
	}

	msg := out.Output[1].OfOutputMessage
	if msg == nil || (*msg.Content)[0].OfOutputText.Text != "let me look that up" {
		t.Errorf("output[1] = %+v, want the assistant text", out.Output[1])
	}

	call := out.Output[2].OfFunctionCall
	if call == nil || call.CallID != "call_1" || call.Name != "weather" {
		t.Fatalf("output[2] = %+v, want the function call", out.Output[2])
	}
	if call.Arguments != `{"city":"pune"}` {
		t.Errorf("arguments = %q", call.Arguments)
	}

	// CachedTokens is a subset of InputTokens, never additional to it.
	if out.Usage.InputTokens != 120 || out.Usage.InputTokensDetails.CachedTokens != 100 {
		t.Errorf("usage input = %d (cached %d), want 120 (cached 100)", out.Usage.InputTokens, out.Usage.InputTokensDetails.CachedTokens)
	}
	if out.Usage.OutputTokensDetails.ReasoningTokens != 12 {
		t.Errorf("reasoning tokens = %d, want 12", out.Usage.OutputTokensDetails.ReasoningTokens)
	}
}

// DeepSeek reports the cached portion of the prompt under its own key rather
// than prompt_tokens_details.
func TestUsageDeepSeekCacheSpelling(t *testing.T) {
	usage := &Usage{PromptTokens: 200, CompletionTokens: 10, PromptCacheHitTokens: 180}

	out := usage.ToNativeUsage()

	if out.InputTokens != 200 || out.InputTokensDetails.CachedTokens != 180 {
		t.Errorf("usage = %+v, want 200 input with 180 cached", out)
	}
	if out.TotalTokens != 210 {
		t.Errorf("total = %d, want 210 (derived when the provider omits it)", out.TotalTokens)
	}
}

func TestStreamConverterTextAndUsage(t *testing.T) {
	converter := NewStreamConverter()

	var chunks []*responses.ResponseChunk
	for _, frame := range []string{
		`{"id":"chatcmpl-1","model":"kimi-k2","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		// Usage arrives in a trailing frame, after finish_reason.
		`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
	} {
		chunk := &ChatResponseChunk{}
		if err := sonic.Unmarshal([]byte(frame), chunk); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		chunks = append(chunks, converter.Convert(chunk)...)
	}
	chunks = append(chunks, converter.Finish()...)

	types := chunkTypes(chunks)
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	assertChunkTypes(t, types, want)

	completed := lastCompleted(t, chunks)
	if completed.Response.Usage.InputTokens != 10 || completed.Response.Usage.TotalTokens != 12 {
		t.Errorf("usage = %+v, want the trailing frame's counts", completed.Response.Usage)
	}
	if len(completed.Response.Output) != 1 {
		t.Fatalf("output = %+v, want one message", completed.Response.Output)
	}
	if text := (*completed.Response.Output[0].OfOutputMessage.Content)[0].OfOutputText.Text; text != "Hello" {
		t.Errorf("accumulated text = %q, want Hello", text)
	}
}

func TestStreamConverterReasoningThenParallelToolCalls(t *testing.T) {
	converter := NewStreamConverter()

	var chunks []*responses.ResponseChunk
	for _, frame := range []string{
		`{"id":"c1","model":"glm-4.6","choices":[{"index":0,"delta":{"reasoning_content":"pick a tool"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pune\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"weather","arguments":"{\"city\":\"delhi\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	} {
		chunk := &ChatResponseChunk{}
		if err := sonic.Unmarshal([]byte(frame), chunk); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		chunks = append(chunks, converter.Convert(chunk)...)
	}
	chunks = append(chunks, converter.Finish()...)

	assertChunkTypes(t, chunkTypes(chunks), []string{
		"response.created",
		"response.in_progress",
		// reasoning item opens, takes its delta...
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		// ...and is closed before the first tool call opens.
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		// switching tool index closes the first call before opening the next.
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	})

	completed := lastCompleted(t, chunks)
	if len(completed.Response.Output) != 3 {
		t.Fatalf("output = %d items, want reasoning + two calls", len(completed.Response.Output))
	}

	first := completed.Response.Output[1].OfFunctionCall
	if first == nil || first.CallID != "call_a" || first.Arguments != `{"city":"pune"}` {
		t.Errorf("first call = %+v, want the fragments reassembled", first)
	}

	second := completed.Response.Output[2].OfFunctionCall
	if second == nil || second.CallID != "call_b" || second.Name != "weather" {
		t.Errorf("second call = %+v", second)
	}

	// Every item must land on its own output index, otherwise the agent loop
	// cannot tell the two calls apart.
	indices := map[int]bool{}
	for _, chunk := range chunks {
		if chunk.OfOutputItemDone != nil {
			indices[chunk.OfOutputItemDone.OutputIndex] = true
		}
	}
	if len(indices) != 3 {
		t.Errorf("distinct output indices = %d, want 3", len(indices))
	}
}

// Not every OpenAI-compatible server puts `index` on tool call deltas. Then
// the id is the only signal: a fragment carrying one starts a call, a
// fragment with only arguments continues the open one.
func TestStreamConverterToolCallsWithoutIndex(t *testing.T) {
	converter := NewStreamConverter()

	for _, frame := range []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"pune\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"time","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	} {
		chunk := &ChatResponseChunk{}
		if err := sonic.Unmarshal([]byte(frame), chunk); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		converter.Convert(chunk)
	}

	completed := lastCompleted(t, converter.Finish())
	output := completed.Response.Output
	if len(output) != 2 {
		t.Fatalf("output = %d items, want 2 calls: %+v", len(output), output)
	}

	if call := output[0].OfFunctionCall; call.CallID != "call_a" || call.Arguments != `{"city":"pune"}` {
		t.Errorf("first call = %+v, want the two fragments joined", call)
	}
	if call := output[1].OfFunctionCall; call.CallID != "call_b" || call.Name != "time" {
		t.Errorf("second call = %+v", call)
	}
}

func TestStreamConverterEmptyStream(t *testing.T) {
	converter := NewStreamConverter()

	// A stream that dies before any frame still has to produce a well-formed
	// envelope, or the caller waits forever for a completion it never gets.
	assertChunkTypes(t, chunkTypes(converter.Finish()), []string{
		"response.created",
		"response.in_progress",
		"response.completed",
	})
}

func chunkTypes(chunks []*responses.ResponseChunk) []string {
	types := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		types = append(types, chunk.ChunkType())
	}

	return types
}

func assertChunkTypes(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("chunk types:\n got %v\nwant %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunk[%d] = %q, want %q\n got %v\nwant %v", i, got[i], want[i], got, want)
		}
	}
}

func lastCompleted(t *testing.T, chunks []*responses.ResponseChunk) *responses.ChunkResponse[constants.ChunkTypeResponseCompleted] {
	t.Helper()

	for i := len(chunks) - 1; i >= 0; i-- {
		if chunks[i].OfResponseCompleted != nil {
			return chunks[i].OfResponseCompleted
		}
	}

	t.Fatal("stream has no response.completed chunk")
	return nil
}
