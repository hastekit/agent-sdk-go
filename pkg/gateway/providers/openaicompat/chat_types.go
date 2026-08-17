// Package openaicompat is a provider client for any service that speaks the
// OpenAI /chat/completions wire format but does not implement the /responses
// API — Sarvam, DeepSeek, Kimi (Moonshot) and GLM (Z.ai) today.
//
// The SDK's native request/response shape is the Responses shape, so this
// package carries a generic translation in both directions:
//
//	responses.Request  -> chat completions request   (responses_to_chat.go)
//	chat completions   -> responses.Response         (chat_to_responses.go)
//	chat SSE chunks    -> responses.ResponseChunk    (chat_to_responses_stream.go)
//
// Prefer a provider's own /responses endpoint when it has one; this bridge
// exists for the ones that don't.
package openaicompat

import (
	"github.com/bytedance/sonic"
)

// ChatRequest is the OpenAI /chat/completions request body. Only the fields
// the Responses shape can express are modelled; ExtraBody carries anything a
// specific provider needs on top (merged in at marshal time by the client).
type ChatRequest struct {
	Model             string            `json:"model"`
	Messages          []Message         `json:"messages"`
	Tools             []Tool            `json:"tools,omitempty"`
	ToolChoice        any               `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	MaxTokens         *int              `json:"max_tokens,omitempty"`
	ReasoningEffort   *string           `json:"reasoning_effort,omitempty"`
	ResponseFormat    map[string]any    `json:"response_format,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Stream            *bool             `json:"stream,omitempty"`
	StreamOptions     *StreamOptions    `json:"stream_options,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Message is one entry of the `messages` array. The same struct also decodes
// the assistant message returned inside a choice.
type Message struct {
	Role      string     `json:"role"`
	Content   Content    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID ties a role="tool" message back to the assistant tool call.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ReasoningContent is the de-facto standard field for exposed chain of
	// thought (DeepSeek, GLM, Kimi thinking models). It is read from
	// responses only — never sent back, since these APIs reject it.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Refusal          string `json:"refusal,omitempty"`
}

// Content is the string-or-parts union used by every message role.
type Content struct {
	OfString *string
	OfParts  []ContentPart
}

func StringContent(s string) Content {
	return Content{OfString: &s}
}

// MarshalJSON must be on the value receiver: Content is held by value inside
// Message, and a pointer-receiver marshaller would be skipped for
// non-addressable values.
func (c Content) MarshalJSON() ([]byte, error) {
	if c.OfString != nil {
		return sonic.Marshal(*c.OfString)
	}

	if c.OfParts != nil {
		return sonic.Marshal(c.OfParts)
	}

	return []byte("null"), nil
}

func (c *Content) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		c.OfString = &s
		return nil
	}

	var parts []ContentPart
	if err := sonic.Unmarshal(data, &parts); err == nil {
		c.OfParts = parts
		return nil
	}

	// null / unknown shape - leave the union empty rather than failing the
	// whole message.
	return nil
}

// Text returns the flattened text of the content, ignoring non-text parts.
func (c Content) Text() string {
	if c.OfString != nil {
		return *c.OfString
	}

	text := ""
	for _, part := range c.OfParts {
		if part.Type == ContentPartTypeText {
			text += part.Text
		}
	}

	return text
}

const (
	ContentPartTypeText     = "text"
	ContentPartTypeImageURL = "image_url"
	ContentPartTypeFile     = "file"
)

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	File     *FilePart `json:"file,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type FilePart struct {
	FileID   *string `json:"file_id,omitempty"`
	Filename *string `json:"filename,omitempty"`
	FileData *string `json:"file_data,omitempty"`
}

type ToolCall struct {
	// Index is present on streaming deltas only, and is what ties argument
	// fragments to the call they belong to.
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"` // "function"
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *Usage       `json:"usage"`
	Error   *ChatError   `json:"error"`
}

type ChatChoice struct {
	Index        int     `json:"index"`
	FinishReason string  `json:"finish_reason"`
	Message      Message `json:"message"`
}

type ChatError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Param   string `json:"param"`
	Code    string `json:"code"`
}

// ChatResponseChunk is one `data:` frame of a streaming chat completion.
type ChatResponseChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []ChatChunkChoice `json:"choices"`
	Usage   *Usage            `json:"usage,omitempty"`
	Error   *ChatError        `json:"error,omitempty"`
}

type ChatChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason string     `json:"finish_reason"`
}

type ChunkDelta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Refusal          string     `json:"refusal,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`

	// PromptCacheHitTokens is DeepSeek's spelling of prompt_tokens_details
	// .cached_tokens. Like it, it is a subset of PromptTokens.
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}
