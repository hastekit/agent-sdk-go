package openaicompat

import (
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// NativeRequestToChatRequest translates a native (Responses-shaped) request
// into a chat completions request.
//
// Items the chat completions API has no equivalent for are dropped rather
// than approximated: reasoning items (these APIs reject an echoed
// reasoning_content), server-side tool calls (web search, image generation,
// code interpreter) and the tools that would produce them.
func NativeRequestToChatRequest(in *responses.Request) *ChatRequest {
	out := &ChatRequest{
		Model:             in.Model,
		Temperature:       in.Temperature,
		TopP:              in.TopP,
		MaxTokens:         in.MaxOutputTokens,
		ParallelToolCalls: in.ParallelToolCalls,
		Metadata:          in.Metadata,
		Stream:            in.Stream,
		ReasoningEffort:   nativeReasoningEffort(in.Reasoning),
		ResponseFormat:    nativeTextFormatToResponseFormat(in.Text),
	}

	if in.IsStreamingRequest() {
		// Without this the final chunk carries no usage, and the run's token
		// accounting - which the history summarizer triggers off - is empty.
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	if in.Instructions != nil && *in.Instructions != "" {
		out.Messages = append(out.Messages, Message{
			Role:    roleSystem,
			Content: StringContent(*in.Instructions),
		})
	}

	if in.Input.OfString != nil {
		out.Messages = append(out.Messages, Message{
			Role:    roleUser,
			Content: StringContent(*in.Input.OfString),
		})
	}

	for _, item := range in.Input.OfInputMessageList {
		out.Messages = appendInputMessage(out.Messages, item)
	}

	for _, tool := range in.Tools {
		if tool.OfFunction == nil {
			continue
		}

		fn := ToolFunction{
			Name:       tool.OfFunction.Name,
			Parameters: tool.OfFunction.Parameters,
			Strict:     tool.OfFunction.Strict,
		}
		if tool.OfFunction.Description != nil {
			fn.Description = *tool.OfFunction.Description
		}

		out.Tools = append(out.Tools, Tool{Type: "function", Function: fn})
	}

	return out
}

func appendInputMessage(msgs []Message, item responses.InputMessageUnion) []Message {
	switch {
	case item.OfEasyInput != nil:
		content := Content{}
		if item.OfEasyInput.Content.OfString != nil {
			content = StringContent(*item.OfEasyInput.Content.OfString)
		} else {
			content = inputContentToContent(item.OfEasyInput.Content.OfInputMessageList)
		}

		return append(msgs, Message{
			Role:    nativeRole(item.OfEasyInput.Role),
			Content: content,
		})

	case item.OfInputMessage != nil:
		return append(msgs, Message{
			Role:    nativeRole(item.OfInputMessage.Role),
			Content: inputContentToContent(item.OfInputMessage.Content),
		})

	case item.OfOutputMessage != nil:
		text := ""
		if item.OfOutputMessage.Content != nil {
			for _, content := range *item.OfOutputMessage.Content {
				if content.OfOutputText != nil {
					text += content.OfOutputText.Text
				}
			}
		}

		return append(msgs, Message{
			Role:    roleAssistant,
			Content: StringContent(text),
		})

	case item.OfFunctionCall != nil:
		call := ToolCall{
			ID:   item.OfFunctionCall.CallID,
			Type: "function",
			Function: ToolCallFunction{
				Name:      item.OfFunctionCall.Name,
				Arguments: item.OfFunctionCall.Arguments,
			},
		}

		// Parallel tool calls arrive as consecutive function_call items but
		// have to go back as a single assistant message carrying all of them,
		// otherwise the tool results that follow have no matching call.
		if n := len(msgs); n > 0 && msgs[n-1].Role == roleAssistant && len(msgs[n-1].ToolCalls) > 0 {
			msgs[n-1].ToolCalls = append(msgs[n-1].ToolCalls, call)
			return msgs
		}

		return append(msgs, Message{Role: roleAssistant, ToolCalls: []ToolCall{call}})

	case item.OfFunctionCallOutput != nil:
		output := ""
		if item.OfFunctionCallOutput.Output.OfString != nil {
			output = *item.OfFunctionCallOutput.Output.OfString
		} else {
			for _, content := range item.OfFunctionCallOutput.Output.OfList {
				if content.OfInputText != nil {
					output += content.OfInputText.Text
				}
				if content.OfOutputText != nil {
					output += content.OfOutputText.Text
				}
			}
		}

		return append(msgs, Message{
			Role:       roleTool,
			ToolCallID: item.OfFunctionCallOutput.CallID,
			Content:    StringContent(output),
		})
	}

	return msgs
}

func inputContentToContent(in responses.InputContent) Content {
	// The common case is plain text; keep it a bare string so providers that
	// only accept the string form of `content` still work.
	textOnly := true
	for _, content := range in {
		if content.OfInputText == nil && content.OfOutputText == nil {
			textOnly = false
			break
		}
	}

	if textOnly {
		text := ""
		for _, content := range in {
			if content.OfInputText != nil {
				text += content.OfInputText.Text
			}
			if content.OfOutputText != nil {
				text += content.OfOutputText.Text
			}
		}

		return StringContent(text)
	}

	parts := make([]ContentPart, 0, len(in))
	for _, content := range in {
		switch {
		case content.OfInputText != nil:
			parts = append(parts, ContentPart{Type: ContentPartTypeText, Text: content.OfInputText.Text})

		case content.OfOutputText != nil:
			parts = append(parts, ContentPart{Type: ContentPartTypeText, Text: content.OfOutputText.Text})

		case content.OfInputImage != nil && content.OfInputImage.ImageURL != nil:
			parts = append(parts, ContentPart{
				Type: ContentPartTypeImageURL,
				ImageURL: &ImageURL{
					URL:    *content.OfInputImage.ImageURL,
					Detail: content.OfInputImage.Detail,
				},
			})

		case content.OfInputFile != nil:
			parts = append(parts, ContentPart{
				Type: ContentPartTypeFile,
				File: &FilePart{
					FileID:   content.OfInputFile.FileID,
					Filename: content.OfInputFile.FileName,
					FileData: content.OfInputFile.FileData,
				},
			})
		}
	}

	return Content{OfParts: parts}
}

func nativeRole(role constants.Role) string {
	switch role {
	case constants.RoleAssistant:
		return roleAssistant
	case constants.RoleSystem, constants.RoleDeveloper:
		// "developer" is an OpenAI-only role; the compatible APIs take
		// "system".
		return roleSystem
	default:
		return roleUser
	}
}

// nativeReasoningEffort maps the native effort scale onto the one chat
// completions accepts. "xhigh" has no equivalent and clamps to "high";
// "none" means reasoning off, which is the absence of the field.
func nativeReasoningEffort(in *responses.ReasoningParam) *string {
	if in == nil || in.Effort == nil {
		return nil
	}

	switch *in.Effort {
	case "low", "medium", "high":
		return in.Effort
	case "xhigh":
		return utils.Ptr("high")
	default:
		return nil
	}
}

// nativeTextFormatToResponseFormat rewrites the Responses text.format object
// into the chat completions response_format object. The two differ in
// nesting: Responses keeps name/schema/strict flat, chat completions wraps
// them in `json_schema`.
func nativeTextFormatToResponseFormat(in *responses.TextFormat) map[string]any {
	if in == nil || in.Format == nil {
		return nil
	}

	formatType, _ := in.Format["type"].(string)
	if formatType != "json_schema" {
		if formatType == "" {
			return nil
		}
		return map[string]any{"type": formatType}
	}

	jsonSchema := map[string]any{}
	for _, key := range []string{"name", "schema", "strict", "description"} {
		if v, ok := in.Format[key]; ok {
			jsonSchema[key] = v
		}
	}

	if _, ok := jsonSchema["name"]; !ok {
		jsonSchema["name"] = "structured_output"
	}

	return map[string]any{
		"type":        "json_schema",
		"json_schema": jsonSchema,
	}
}
