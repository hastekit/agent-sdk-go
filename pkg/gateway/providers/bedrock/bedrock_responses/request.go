package bedrock_responses

import (
	"encoding/base64"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// ConverseRequest is the top-level request body for Bedrock Converse API.
type ConverseRequest struct {
	Messages                     []ConverseMessage `json:"messages"`
	System                       []SystemContent   `json:"system,omitempty"`
	InferenceConfig              *InferenceConfig  `json:"inferenceConfig,omitempty"`
	ToolConfig                   *ToolConfig       `json:"toolConfig,omitempty"`
	AdditionalModelRequestFields map[string]any    `json:"additionalModelRequestFields,omitempty"`
}

type SystemContent struct {
	Text string `json:"text"`
}

type InferenceConfig struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type ToolConfig struct {
	Tools []Tool `json:"tools"`
}

type Tool struct {
	ToolSpec   *ToolSpec        `json:"toolSpec,omitempty"`
	CachePoint *CachePointBlock `json:"cachePoint,omitempty"`
}

// CachePointBlock marks a Bedrock Converse cache breakpoint. The Bedrock
// translator converts this into the underlying model's native caching
// directive (e.g. Anthropic's cache_control).
//
// Type currently has only one valid value, "default". TTL is optional;
// "5m" (default) and "1h" are accepted, but "1h" is supported only on a
// subset of models (Claude Opus/Sonnet/Haiku 4.5).
type CachePointBlock struct {
	Type string `json:"type"`          // "default"
	TTL  string `json:"ttl,omitempty"` // "5m" or "1h"
}

type ToolSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	JSON map[string]any `json:"json,omitempty"`
}

// ConverseMessage represents a message in the Converse API.
type ConverseMessage struct {
	Role    string         `json:"role"` // "user" or "assistant"
	Content []ContentBlock `json:"content"`
}

// ContentBlock is a union type for content blocks in a Converse message.
type ContentBlock struct {
	Text             *string           `json:"text,omitempty"`
	Image            *ImageBlock       `json:"image,omitempty"`
	Document         *DocumentBlock    `json:"document,omitempty"`
	ToolUse          *ToolUseBlock     `json:"toolUse,omitempty"`
	ToolResult       *ToolResultBlock  `json:"toolResult,omitempty"`
	ReasoningContent *ReasoningContent `json:"reasoningContent,omitempty"`
	CachePoint       *CachePointBlock  `json:"cachePoint,omitempty"`
}

// ReasoningContent represents a reasoning/thinking content block in the Converse API.
type ReasoningContent struct {
	ReasoningText   *ReasoningText `json:"reasoningText,omitempty"`
	RedactedContent *string        `json:"redactedContent,omitempty"`
}

type ReasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

type ImageBlock struct {
	Format string           `json:"format"` // "png", "jpeg", "gif", "webp"
	Source ImageSourceBlock `json:"source"`
}

type ImageSourceBlock struct {
	Bytes string `json:"bytes"` // base64-encoded image bytes
}

type DocumentBlock struct {
	Format string              `json:"format"` // "pdf", "csv", "doc", "docx", "html", "txt", "md", "xls", "xlsx"
	Name   string              `json:"name"`
	Source DocumentSourceBlock `json:"source"`
}

type DocumentSourceBlock struct {
	Bytes string `json:"bytes"` // base64-encoded document bytes
}

type ToolUseBlock struct {
	ToolUseId string `json:"toolUseId"`
	Name      string `json:"name"`
	Input     any    `json:"input"`
}

type ToolResultBlock struct {
	ToolUseId string         `json:"toolUseId"`
	Content   []ContentBlock `json:"content"`
	Status    string         `json:"status,omitempty"` // "success" or "error"
}

// NativeRequestToConverseRequest converts a native SDK request to a Bedrock Converse API request.
func NativeRequestToConverseRequest(in *responses.Request) *ConverseRequest {
	if in.MaxOutputTokens == nil {
		in.MaxOutputTokens = utils.Ptr(1512)
	}

	if in.MaxToolCalls != nil {
		slog.Warn("max tool calls is not supported for bedrock converse")
	}

	if in.ParallelToolCalls != nil {
		slog.Warn("parallel tool calls is not supported for bedrock converse")
	}

	out := &ConverseRequest{
		Messages: nativeInputToConverseMessages(in.Input),
		InferenceConfig: &InferenceConfig{
			MaxTokens:   in.MaxOutputTokens,
			Temperature: in.Temperature,
			TopP:        in.TopP,
		},
	}

	// Add reasoning/thinking config via additionalModelRequestFields
	if in.Reasoning != nil && in.Reasoning.Effort != nil && *in.Reasoning.Effort != "none" {
		budgetTokens := effortToBudgetTokens(*in.Reasoning.Effort)
		out.AdditionalModelRequestFields = map[string]any{
			"thinking": map[string]any{
				"type":          "enabled",
				"budget_tokens": budgetTokens,
			},
		}
	}

	if in.Instructions != nil && *in.Instructions != "" {
		out.System = []SystemContent{
			{Text: *in.Instructions},
		}
	}

	tools := nativeToolsToConverseTools(in.Tools)
	if len(tools) > 0 {
		out.ToolConfig = &ToolConfig{Tools: tools}
	}

	var cacheEnabled bool
	var cacheTTL string
	if in.ExtraFields != nil {
		if v, ok := in.ExtraFields["cache_strategy"].(string); ok && v != "" {
			cacheEnabled = true
		}
		if v, ok := in.ExtraFields["cache_ttl"].(string); ok {
			cacheTTL = v
		}

		if out.AdditionalModelRequestFields == nil {
			out.AdditionalModelRequestFields = map[string]any{}
		}
		for k, v := range in.ExtraFields {
			// Meta-fields consumed by the SDK itself, not forwarded to Bedrock:
			//   additional_headers — transport-level, applied by AddAdditionalHeaders
			//   cache_strategy     — enables cachePoint injection below
			//   cache_ttl          — sets ttl on injected cachePoints
			if k == "additional_headers" || k == "cache_strategy" || k == "cache_ttl" {
				continue
			}
			out.AdditionalModelRequestFields[k] = v
		}
	}

	if cacheEnabled {
		injectCachePoints(out, cacheTTL)
	}

	return out
}

// injectCachePoints adds Bedrock-native cachePoint blocks at the standard
// breakpoints: the end of the last user message's content, and the end of the
// tool list. Mirrors the Strands SDK default behavior for Anthropic models on
// Bedrock — the Converse translator converts these into the model's native
// caching directives.
//
// ttl is optional; pass "" to use Bedrock's default (5m), "5m", or "1h".
func injectCachePoints(req *ConverseRequest, ttl string) {
	point := func() *CachePointBlock {
		return &CachePointBlock{Type: "default", TTL: ttl}
	}

	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			req.Messages[i].Content = append(req.Messages[i].Content, ContentBlock{
				CachePoint: point(),
			})
			break
		}
	}

	if req.ToolConfig != nil && len(req.ToolConfig.Tools) > 0 {
		req.ToolConfig.Tools = append(req.ToolConfig.Tools, Tool{
			CachePoint: point(),
		})
	}
}

func nativeToolsToConverseTools(nativeTools []responses.ToolUnion) []Tool {
	var out []Tool

	for _, nativeTool := range nativeTools {
		if nativeTool.OfFunction != nil {
			desc := ""
			if nativeTool.OfFunction.Description != nil {
				desc = *nativeTool.OfFunction.Description
			}

			out = append(out, Tool{
				ToolSpec: &ToolSpec{
					Name:        nativeTool.OfFunction.Name,
					Description: desc,
					InputSchema: InputSchema{
						JSON: nativeTool.OfFunction.Parameters,
					},
				},
			})
		}

		if nativeTool.OfWebSearch != nil {
			slog.Warn("web search tool is not supported for bedrock converse, skipping")
		}

		if nativeTool.OfCodeExecution != nil {
			slog.Warn("code execution tool is not supported for bedrock converse, skipping")
		}
	}

	return out
}

func nativeInputToConverseMessages(in responses.InputUnion) []ConverseMessage {
	var out []ConverseMessage

	if in.OfString != nil {
		out = append(out, ConverseMessage{
			Role:    "user",
			Content: []ContentBlock{{Text: utils.Ptr(*in.OfString)}},
		})
		return out
	}

	if in.OfInputMessageList == nil {
		return out
	}

	for _, nativeMsg := range in.OfInputMessageList {
		if nativeMsg.OfEasyInput != nil {
			msg := convertEasyMessage(nativeMsg.OfEasyInput)
			if msg != nil {
				out = append(out, *msg)
			}
		}

		if nativeMsg.OfInputMessage != nil {
			msg := convertInputMessage(nativeMsg.OfInputMessage)
			if msg != nil {
				out = append(out, *msg)
			}
		}

		if nativeMsg.OfOutputMessage != nil {
			msg := convertOutputMessage(nativeMsg.OfOutputMessage)
			if msg != nil {
				out = append(out, *msg)
			}
		}

		if nativeMsg.OfFunctionCall != nil {
			out = append(out, convertFunctionCallToAssistant(nativeMsg.OfFunctionCall))
		}

		if nativeMsg.OfFunctionCallOutput != nil {
			out = append(out, convertFunctionCallOutputToUser(nativeMsg.OfFunctionCallOutput))
		}

		if nativeMsg.OfReasoning != nil {
			msg := convertReasoningToAssistant(nativeMsg.OfReasoning)
			if msg != nil {
				out = append(out, *msg)
			}
		}
	}

	// Merge consecutive messages with the same role
	out = mergeConsecutiveMessages(out)

	return out
}

func convertEasyMessage(msg *responses.EasyMessage) *ConverseMessage {
	var blocks []ContentBlock

	if msg.Content.OfString != nil {
		blocks = append(blocks, ContentBlock{Text: msg.Content.OfString})
	}

	if msg.Content.OfInputMessageList != nil {
		blocks = append(blocks, convertInputContent(msg.Content.OfInputMessageList)...)
	}

	if len(blocks) == 0 {
		return nil
	}

	return &ConverseMessage{
		Role:    nativeRoleToConverseRole(msg.Role),
		Content: blocks,
	}
}

func convertInputMessage(msg *responses.InputMessage) *ConverseMessage {
	blocks := convertInputContent(msg.Content)
	if len(blocks) == 0 {
		return nil
	}

	return &ConverseMessage{
		Role:    nativeRoleToConverseRole(msg.Role),
		Content: blocks,
	}
}

func convertOutputMessage(msg *responses.OutputMessage) *ConverseMessage {
	var blocks []ContentBlock

	if msg.Content != nil {
		for _, c := range *msg.Content {
			if c.OfOutputText != nil {
				blocks = append(blocks, ContentBlock{Text: utils.Ptr(c.OfOutputText.Text)})
			}
		}
	}

	if len(blocks) == 0 {
		return nil
	}

	return &ConverseMessage{
		Role:    "assistant",
		Content: blocks,
	}
}

func convertFunctionCallToAssistant(fc *responses.FunctionCallMessage) ConverseMessage {
	args := map[string]any{}
	if err := sonic.Unmarshal([]byte(fc.Arguments), &args); err != nil {
		slog.Warn("unable to unmarshal tool_use args", slog.Any("error", err))
	}

	return ConverseMessage{
		Role: "assistant",
		Content: []ContentBlock{
			{
				ToolUse: &ToolUseBlock{
					ToolUseId: fc.CallID,
					Name:      fc.Name,
					Input:     args,
				},
			},
		},
	}
}

func convertFunctionCallOutputToUser(fco *responses.FunctionCallOutputMessage) ConverseMessage {
	var resultContent []ContentBlock

	if fco.Output.OfString != nil {
		resultContent = append(resultContent, ContentBlock{Text: fco.Output.OfString})
	}

	// A tool result carries the same attachments a message does — an MCP tool
	// returning a screenshot, a chart, a generated picture — and Converse's
	// toolResult takes the same blocks, so they need no special case beyond
	// being looked for at all.
	for _, c := range fco.Output.OfList {
		switch {
		case c.OfInputText != nil:
			resultContent = append(resultContent, ContentBlock{Text: utils.Ptr(c.OfInputText.Text)})
		case c.OfOutputText != nil:
			resultContent = append(resultContent, ContentBlock{Text: utils.Ptr(c.OfOutputText.Text)})
		case c.OfInputImage != nil:
			resultContent = append(resultContent, nativeImageToBlock(c.OfInputImage))
		case c.OfInputFile != nil:
			resultContent = append(resultContent, nativeFileToBlock(c.OfInputFile))
		}
	}

	if len(resultContent) == 0 {
		resultContent = append(resultContent, ContentBlock{Text: utils.Ptr("")})
	}

	return ConverseMessage{
		Role: "user",
		Content: []ContentBlock{
			{
				ToolResult: &ToolResultBlock{
					ToolUseId: fco.CallID,
					Content:   resultContent,
				},
			},
		},
	}
}

func convertReasoningToAssistant(rm *responses.ReasoningMessage) *ConverseMessage {
	var blocks []ContentBlock

	// If we have encrypted content (signature), send as reasoning with signature
	if rm.EncryptedContent != nil && *rm.EncryptedContent != "" {
		summaryText := ""
		if len(rm.Summary) > 0 {
			summaryText = rm.Summary[0].Text
		}
		blocks = append(blocks, ContentBlock{
			ReasoningContent: &ReasoningContent{
				ReasoningText: &ReasoningText{
					Text:      summaryText,
					Signature: *rm.EncryptedContent,
				},
			},
		})
	} else if len(rm.Summary) > 0 {
		// Summary text only (no signature)
		for _, s := range rm.Summary {
			blocks = append(blocks, ContentBlock{
				ReasoningContent: &ReasoningContent{
					ReasoningText: &ReasoningText{
						Text: s.Text,
					},
				},
			})
		}
	}

	if len(blocks) == 0 {
		return nil
	}

	return &ConverseMessage{
		Role:    "assistant",
		Content: blocks,
	}
}

func convertInputContent(content responses.InputContent) []ContentBlock {
	var blocks []ContentBlock

	for _, c := range content {
		if c.OfInputText != nil {
			blocks = append(blocks, ContentBlock{Text: utils.Ptr(c.OfInputText.Text)})
		}

		if c.OfOutputText != nil {
			blocks = append(blocks, ContentBlock{Text: utils.Ptr(c.OfOutputText.Text)})
		}

		if c.OfInputImage != nil {
			blocks = append(blocks, nativeImageToBlock(c.OfInputImage))
		}

		if c.OfInputFile != nil {
			blocks = append(blocks, nativeFileToBlock(c.OfInputFile))
		}
	}

	return blocks
}

// Converse carries an attachment exactly one way: the bytes, inline. There is
// no URL it will fetch and no file store to reference, so anything that is not
// already bytes cannot be sent — and says so, rather than going missing, which
// is what these used to do.

func nativeImageToBlock(img *responses.InputImageContent) ContentBlock {
	if img.FileID != nil && *img.FileID != "" {
		return undeliverable("an image", "this model takes image data inline and cannot resolve a file id")
	}

	if img.ImageURL == nil || *img.ImageURL == "" {
		return undeliverable("an image", "it carried neither image data, a URL, nor a file id")
	}

	if !strings.HasPrefix(*img.ImageURL, "data:") {
		return undeliverable("an image", "this model takes image data inline and will not fetch a URL")
	}

	contentType, data, err := utils.ParseDataURL(*img.ImageURL)
	if err != nil {
		slog.Error("bedrock: image data URL could not be parsed", slog.Any("error", err))
		return undeliverable("an image", "its inline data could not be read")
	}

	format := "png"
	switch {
	case strings.Contains(contentType, "jpeg"), strings.Contains(contentType, "jpg"):
		format = "jpeg"
	case strings.Contains(contentType, "gif"):
		format = "gif"
	case strings.Contains(contentType, "webp"):
		format = "webp"
	}

	// data from ParseDataURL is already base64-encoded
	// Converse API expects raw bytes base64-encoded
	return ContentBlock{Image: &ImageBlock{
		Format: format,
		Source: ImageSourceBlock{Bytes: data},
	}}
}

func nativeFileToBlock(file *responses.InputFileContent) ContentBlock {
	name := "document"
	if file.FileName != nil && *file.FileName != "" {
		name = *file.FileName
	}

	if file.FileID != nil && *file.FileID != "" {
		return undeliverable("a file", "this model takes file data inline and cannot resolve a file id")
	}

	if file.FileData == nil || *file.FileData == "" {
		if file.FileURL != nil && *file.FileURL != "" {
			return undeliverable("a file", "this model takes file data inline and will not fetch a URL")
		}
		return undeliverable("a file", "it carried neither file data, a URL, nor a file id")
	}

	data := *file.FileData
	format := documentFormat(name)
	if strings.HasPrefix(data, "data:") {
		contentType, encoded, err := utils.ParseDataURL(data)
		if err != nil {
			slog.Error("bedrock: file data URL could not be parsed", slog.Any("error", err))
			return undeliverable("a file", "its inline data could not be read")
		}
		data = encoded
		if f := documentFormatFromMediaType(contentType); f != "" {
			format = f
		}
	}

	return ContentBlock{Document: &DocumentBlock{
		Format: format,
		Name:   name,
		Source: DocumentSourceBlock{Bytes: data},
	}}
}

func undeliverable(kind, reason string) ContentBlock {
	slog.Error("bedrock: attachment dropped from the request",
		slog.String("kind", kind), slog.String("reason", reason))
	return ContentBlock{Text: utils.Ptr(responses.UndeliverableAttachment(kind, reason))}
}

// documentFormat is Converse's own vocabulary, which is a bare word rather
// than a media type. pdf is the fallback because it is what these are.
func documentFormat(filename string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), ".")) {
	case "csv":
		return "csv"
	case "doc":
		return "doc"
	case "docx":
		return "docx"
	case "html", "htm":
		return "html"
	case "txt":
		return "txt"
	case "md", "markdown":
		return "md"
	case "xls":
		return "xls"
	case "xlsx":
		return "xlsx"
	default:
		return "pdf"
	}
}

func documentFormatFromMediaType(mediaType string) string {
	switch {
	case strings.Contains(mediaType, "pdf"):
		return "pdf"
	case strings.Contains(mediaType, "csv"):
		return "csv"
	case strings.Contains(mediaType, "html"):
		return "html"
	case strings.Contains(mediaType, "markdown"):
		return "md"
	case strings.Contains(mediaType, "text/plain"):
		return "txt"
	case strings.Contains(mediaType, "wordprocessingml"):
		return "docx"
	case strings.Contains(mediaType, "spreadsheetml"):
		return "xlsx"
	case strings.Contains(mediaType, "msword"):
		return "doc"
	case strings.Contains(mediaType, "ms-excel"):
		return "xls"
	default:
		return ""
	}
}

func nativeRoleToConverseRole(role constants.Role) string {
	switch role {
	case constants.RoleAssistant:
		return "assistant"
	default:
		return "user"
	}
}

// mergeConsecutiveMessages merges consecutive messages with the same role.
// Converse API requires alternating user/assistant roles.
func mergeConsecutiveMessages(msgs []ConverseMessage) []ConverseMessage {
	if len(msgs) <= 1 {
		return msgs
	}

	var merged []ConverseMessage
	current := msgs[0]

	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == current.Role {
			current.Content = append(current.Content, msgs[i].Content...)
		} else {
			merged = append(merged, current)
			current = msgs[i]
		}
	}
	merged = append(merged, current)

	return merged
}

// effortToBudgetTokens maps native reasoning effort levels to Bedrock budget_tokens.
func effortToBudgetTokens(effort string) int {
	switch effort {
	case "low":
		return 1024
	case "medium":
		return 3000
	case "high":
		return 6000
	case "xhigh":
		return 10000
	default:
		return 3000
	}
}

// ImageBytesToBase64 converts raw image bytes to base64 string.
func ImageBytesToBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
