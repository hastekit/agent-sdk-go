package openaicompat

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

func TestNativeRequestToChatRequestMessages(t *testing.T) {
	req := &responses.Request{
		Model:        "sarvam-105b",
		Instructions: utils.Ptr("be terse"),
		Input: responses.InputUnion{
			OfInputMessageList: responses.InputMessageList{
				responses.UserMessage("what is the weather"),
				{OfFunctionCall: &responses.FunctionCallMessage{CallID: "call_1", Name: "weather", Arguments: `{"city":"pune"}`}},
				{OfFunctionCall: &responses.FunctionCallMessage{CallID: "call_2", Name: "weather", Arguments: `{"city":"delhi"}`}},
				{OfFunctionCallOutput: &responses.FunctionCallOutputMessage{CallID: "call_1", Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("32C")}}},
				{OfFunctionCallOutput: &responses.FunctionCallOutputMessage{CallID: "call_2", Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr("28C")}}},
				// Reasoning has no chat completions equivalent and these APIs
				// reject it being echoed back.
				{OfReasoning: &responses.ReasoningMessage{Summary: []responses.SummaryTextContent{{Text: "thinking"}}}},
				{OfOutputMessage: &responses.OutputMessage{Role: constants.RoleAssistant, Content: &responses.OutputContent{
					{OfOutputText: &responses.OutputTextContent{Text: "it is warm"}},
				}}},
			},
		},
	}

	out := NativeRequestToChatRequest(req)

	// system, user, one assistant carrying both calls, two tool results and
	// the final assistant text - the reasoning item is dropped.
	if len(out.Messages) != 6 {
		t.Fatalf("got %d messages, want 6: %+v", len(out.Messages), out.Messages)
	}

	if out.Messages[0].Role != roleSystem || out.Messages[0].Content.Text() != "be terse" {
		t.Errorf("message[0] = %+v, want system instructions", out.Messages[0])
	}

	if out.Messages[1].Role != roleUser || out.Messages[1].Content.Text() != "what is the weather" {
		t.Errorf("message[1] = %+v, want the user turn", out.Messages[1])
	}

	// Consecutive function calls have to collapse into one assistant message:
	// a tool result whose call is not on the preceding assistant message is
	// rejected by every OpenAI-compatible API.
	assistant := out.Messages[2]
	if assistant.Role != roleAssistant || len(assistant.ToolCalls) != 2 {
		t.Fatalf("message[2] = %+v, want one assistant message carrying both tool calls", assistant)
	}
	if assistant.ToolCalls[0].ID != "call_1" || assistant.ToolCalls[1].ID != "call_2" {
		t.Errorf("tool call ids = %q/%q, want call_1/call_2", assistant.ToolCalls[0].ID, assistant.ToolCalls[1].ID)
	}
	if assistant.ToolCalls[0].Function.Arguments != `{"city":"pune"}` {
		t.Errorf("tool call args = %q", assistant.ToolCalls[0].Function.Arguments)
	}

	for i, want := range []struct{ id, content string }{{"call_1", "32C"}, {"call_2", "28C"}} {
		msg := out.Messages[3+i]
		if msg.Role != roleTool || msg.ToolCallID != want.id || msg.Content.Text() != want.content {
			t.Errorf("message[%d] = %+v, want tool result %s", 3+i, msg, want.id)
		}
	}

	if final := out.Messages[5]; final.Role != roleAssistant || final.Content.Text() != "it is warm" {
		t.Errorf("message[5] = %+v, want the assistant's final text", final)
	}
}

func TestNativeRequestToChatRequestParameters(t *testing.T) {
	req := &responses.Request{
		Model: "deepseek-chat",
		Input: responses.InputUnion{OfString: utils.Ptr("hi")},
		Tools: []responses.ToolUnion{
			{OfFunction: &responses.FunctionTool{
				Name:        "weather",
				Description: utils.Ptr("look up the weather"),
				Parameters:  map[string]any{"type": "object"},
			}},
			// Server-side tools cannot be expressed and must be dropped, not
			// sent as a malformed function.
			{OfWebSearch: &responses.WebSearchTool{}},
		},
		Parameters: responses.Parameters{
			Temperature:     utils.Ptr(0.4),
			MaxOutputTokens: utils.Ptr(512),
			Stream:          utils.Ptr(true),
			Reasoning:       &responses.ReasoningParam{Effort: utils.Ptr("xhigh")},
			Text: &responses.TextFormat{Format: map[string]any{
				"type":   "json_schema",
				"name":   "structured_output",
				"strict": true,
				"schema": map[string]any{"type": "object"},
			}},
		},
	}

	out := NativeRequestToChatRequest(req)

	if len(out.Messages) != 1 || out.Messages[0].Role != roleUser {
		t.Fatalf("messages = %+v, want a single user message", out.Messages)
	}

	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "weather" {
		t.Fatalf("tools = %+v, want only the function tool", out.Tools)
	}
	if out.Tools[0].Function.Description != "look up the weather" {
		t.Errorf("tool description = %q", out.Tools[0].Function.Description)
	}

	if out.MaxTokens == nil || *out.MaxTokens != 512 {
		t.Errorf("MaxTokens = %v, want 512", out.MaxTokens)
	}

	// xhigh has no chat completions equivalent and clamps to high.
	if out.ReasoningEffort == nil || *out.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %v, want high", out.ReasoningEffort)
	}

	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Error("streaming request must ask for usage, otherwise the run reports no tokens")
	}

	// Responses keeps name/schema flat; chat completions nests them under
	// json_schema.
	schema, ok := out.ResponseFormat["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("ResponseFormat = %+v, want a json_schema wrapper", out.ResponseFormat)
	}
	if schema["name"] != "structured_output" || schema["strict"] != true {
		t.Errorf("json_schema = %+v", schema)
	}
	if _, ok := schema["schema"]; !ok {
		t.Errorf("json_schema is missing the schema itself: %+v", schema)
	}
}

func TestNativeRequestToChatRequestMultimodalContent(t *testing.T) {
	req := &responses.Request{
		Model: "glm-4.6v",
		Input: responses.InputUnion{OfInputMessageList: responses.InputMessageList{
			{OfInputMessage: &responses.InputMessage{Role: constants.RoleUser, Content: responses.InputContent{
				{OfInputText: &responses.InputTextContent{Text: "what is this"}},
				{OfInputImage: &responses.InputImageContent{ImageURL: utils.Ptr("https://example.com/a.png"), Detail: "high"}},
			}}},
		}},
	}

	out := NativeRequestToChatRequest(req)

	parts := out.Messages[0].Content.OfParts
	if len(parts) != 2 {
		t.Fatalf("content parts = %+v, want text + image", parts)
	}
	if parts[0].Type != ContentPartTypeText || parts[1].Type != ContentPartTypeImageURL {
		t.Fatalf("part types = %q/%q", parts[0].Type, parts[1].Type)
	}
	if parts[1].ImageURL.URL != "https://example.com/a.png" || parts[1].ImageURL.Detail != "high" {
		t.Errorf("image part = %+v", parts[1].ImageURL)
	}
}
