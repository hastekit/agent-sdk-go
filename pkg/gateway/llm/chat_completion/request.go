package chat_completion

import (
	"errors"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
)

type Request struct {
	Messages            []ChatCompletionMessageUnion `json:"messages"`
	Model               string                       `json:"model"`
	FrequencyPenalty    *float64                     `json:"frequency_penalty,omitempty"`
	Logprobs            *bool                        `json:"logprobs,omitempty"`
	MaxCompletionTokens *int64                       `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int64                       `json:"max_tokens,omitempty"` // Deprecated in favour of `MaxCompletionTokens`
	N                   *int64                       `json:"n,omitempty"`          // How many choices needs to be generated
	PresencePenalty     *float64                     `json:"presence_penalty,omitempty"`
	Seed                *int64                       `json:"seed,omitempty"`
	Store               *bool                        `json:"store,omitempty"`
	Temperature         *float64                     `json:"temperature,omitempty"`
	TopLogprobs         *int64                       `json:"top_logprobs,omitempty"`
	TopP                *float64                     `json:"top_p,omitempty"`
	ParallelToolCalls   *bool                        `json:"parallel_tool_calls,omitempty"`
	PromptCacheKey      *string                      `json:"prompt_cache_key,omitempty"`
	SafetyIdentifier    *string                      `json:"safety_identifier,omitempty"`
	User                *string                      `json:"user,omitempty"`
	Audio               *AudioParam                  `json:"audio,omitempty"`
	LogitBias           map[string]int64             `json:"logit_bias,omitempty"`
	Metadata            map[string]string            `json:"metadata,omitempty"`
	Modalities          []string                     `json:"modalities,omitempty"`       // "text", "audio"
	ReasoningEffort     *string                      `json:"reasoning_effort,omitempty"` // "minimal", "low", "medium", "high"
	ServiceTier         *string                      `json:"service_tier,omitempty"`     // "auto", "default", "flex", "scale", "priority"
	Stop                *StopParam                   `json:"stop,omitempty"`
	Stream              *bool                        `json:"stream,omitempty"`
	StreamOptions       *StreamOptionParam           `json:"stream_options,omitempty"` // Set only when setting stream=true
	Verbosity           *string                      `json:"verbosity,omitempty"`      // "low", "medium", "high"
	FunctionCall        *FunctionCallParam           `json:"function_call,omitempty"`  // Deprecated in favour of `tool_choice`
	Functions           []FunctionsParam             `json:"functions,omitempty"`      // Deprecated in favour of tools
	Prediction          any                          `json:"prediction,omitempty"`
	ResponseFormat      any                          `json:"response_format,omitempty"`
	ToolChoice          *string                      `json:"tool_choice,omitempty"`
	Tools               any                          `json:"tools,omitempty"`
	WebSearchOptions    any                          `json:"web_search_options,omitempty"`
}

func (s *Request) IsStreamingRequest() bool {
	if s.Stream == nil {
		return false
	}

	return *s.Stream
}

type AudioParam struct {
	Voice  string `json:"voice"`
	Format string `json:"format"`
}

type StopParam struct {
	OfString *string  `json:",omitempty"`
	OfList   []string `json:",omitempty"`
}

type StreamOptionParam struct {
	IncludeObfuscation *bool `json:"include_obfuscation,omitempty"`
	IncludeUsage       *bool `json:"include_usage,omitempty"`
}

type FunctionCallParam struct {
	OfFunctionCallMode   *string                  `json:",omitempty"`
	OfFunctionCallOption *FunctionCallOptionParam `json:",omitempty"`
}

type FunctionCallOptionParam struct {
	Name string `json:"name"`
}

type FunctionsParam struct {
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ChatCompletionMessageUnion struct {
	OfDeveloper *DeveloperChatCompletionMessageUnion `json:",omitzero"`
	OfSystem    *SystemChatCompletionMessageUnion    `json:",omitzero"`
	OfUser      *UserChatCompletionMessageUnion      `json:",omitzero"`
	OfAssistant *AssistantChatCompletionMessageUnion `json:",omitzero"`
	OfTool      *ToolChatCompletionMessageUnion      `json:",omitzero"`
	OfFunction  *FunctionChatCompletionMessageUnion  `json:",omitzero"`
}

func (u *ChatCompletionMessageUnion) UnmarshalJSON(data []byte) error {
	// First, try to unmarshal as a map to check the role field
	var raw map[string]any
	if err := sonic.Unmarshal(data, &raw); err != nil {
		return err
	}

	role, ok := raw["role"].(string)
	if !ok {
		return errors.New("invalid message: missing role field")
	}

	switch constants.Role(role) {
	case constants.RoleDeveloper:
		var msg DeveloperChatCompletionMessageUnion
		if err := sonic.Unmarshal(data, &msg); err != nil {
			return err
		}
		u.OfDeveloper = &msg
		return nil
	case constants.RoleSystem:
		var msg SystemChatCompletionMessageUnion
		if err := sonic.Unmarshal(data, &msg); err != nil {
			return err
		}
		u.OfSystem = &msg
		return nil
	case constants.RoleUser:
		var msg UserChatCompletionMessageUnion
		if err := sonic.Unmarshal(data, &msg); err != nil {
			return err
		}
		u.OfUser = &msg
		return nil
	case constants.RoleAssistant:
		var msg AssistantChatCompletionMessageUnion
		if err := sonic.Unmarshal(data, &msg); err != nil {
			return err
		}
		u.OfAssistant = &msg
		return nil
	case RoleTool:
		var msg ToolChatCompletionMessageUnion
		if err := sonic.Unmarshal(data, &msg); err != nil {
			return err
		}
		u.OfTool = &msg
		return nil
	default:
		// Try function role (legacy)
		var msg FunctionChatCompletionMessageUnion
		if err := sonic.Unmarshal(data, &msg); err == nil && msg.Role != "" {
			u.OfFunction = &msg
			return nil
		}
		return errors.New("invalid message: unknown role")
	}
}

func (u *ChatCompletionMessageUnion) MarshalJSON() ([]byte, error) {
	if u.OfDeveloper != nil {
		return sonic.Marshal(u.OfDeveloper)
	}

	if u.OfSystem != nil {
		return sonic.Marshal(u.OfSystem)
	}

	if u.OfUser != nil {
		return sonic.Marshal(u.OfUser)
	}

	if u.OfAssistant != nil {
		return sonic.Marshal(u.OfAssistant)
	}

	if u.OfTool != nil {
		return sonic.Marshal(u.OfTool)
	}

	if u.OfFunction != nil {
		return sonic.Marshal(u.OfFunction)
	}

	return nil, nil
}

type DeveloperChatCompletionMessageUnion struct {
	Name    *string                      `json:"name,omitempty"`
	Role    constants.Role               `json:"role,omitempty"` // "developer"
	Content DeveloperMessageContentUnion `json:"content,omitempty"`
}

type SystemChatCompletionMessageUnion struct {
	Name    *string                   `json:"name,omitempty"`
	Role    constants.Role            `json:"role,omitempty"` // system
	Content SystemMessageContentUnion `json:"content,omitempty"`
}

type UserChatCompletionMessageUnion struct {
	Name    *string                 `json:"name,omitempty"`
	Role    constants.Role          `json:"role,omitempty"` // user
	Content UserMessageContentUnion `json:"content,omitempty"`
}

type AssistantChatCompletionMessageUnion struct {
	Refusal      *string                         `json:"refusal,omitempty"`
	Name         *string                         `json:"name,omitempty"`
	Audio        AssistantMessageAudio           `json:"audio,omitempty"`
	Content      AssistantMessageContentUnion    `json:"content,omitempty"`
	FunctionCall AssistantMessageFunctionCall    `json:"function_call,omitempty"`
	ToolCalls    []AssistantMessageToolCallUnion `json:"tool_calls,omitempty"`
	Role         *string                         `json:"role,omitempty"` // "assistant"
}
type ToolChatCompletionMessageUnion struct {
	Role       constants.Role          `json:"role,omitempty"` // tool
	Content    ToolMessageContentUnion `json:"content,omitempty"`
	ToolCallID string                  `json:"tool_call_id,omitempty"`
}
type FunctionChatCompletionMessageUnion struct {
	Name    *string        `json:"name,omitempty"`
	Role    constants.Role `json:"role,omitempty"` //
	Content *string        `json:"content,omitempty"`
}

type DeveloperMessageContentUnion struct {
	OfString *string    `json:",omitempty"`
	OfList   []TextPart `json:",omitempty"`
}

func (u *DeveloperMessageContentUnion) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		u.OfString = &s
		return nil
	}

	var list []TextPart
	if err := sonic.Unmarshal(data, &list); err == nil {
		u.OfList = list
		return nil
	}

	return errors.New("invalid developer message content union")
}

func (u *DeveloperMessageContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	if u.OfList != nil {
		return sonic.Marshal(u.OfList)
	}

	return nil, nil
}

type SystemMessageContentUnion struct {
	OfString *string    `json:",omitempty"`
	OfList   []TextPart `json:",omitempty"`
}

func (u *SystemMessageContentUnion) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		u.OfString = &s
		return nil
	}

	var list []TextPart
	if err := sonic.Unmarshal(data, &list); err == nil {
		u.OfList = list
		return nil
	}

	return errors.New("invalid system message content union")
}

func (u *SystemMessageContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	if u.OfList != nil {
		return sonic.Marshal(u.OfList)
	}

	return nil, nil
}

type UserMessageContentUnion struct {
	OfString *string                       `json:",omitempty"`
	OfList   []UserMessageContentPartUnion `json:",omitempty"`
}

func (u *UserMessageContentUnion) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		u.OfString = &s
		return nil
	}

	var list []UserMessageContentPartUnion
	if err := sonic.Unmarshal(data, &list); err == nil {
		u.OfList = list
		return nil
	}

	return errors.New("invalid user message content union")
}

func (u *UserMessageContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	if u.OfList != nil {
		return sonic.Marshal(u.OfList)
	}

	return nil, nil
}

type AssistantMessageAudio struct {
	ID string `json:"id"`
}

type AssistantMessageContentUnion struct {
	OfString *string                            `json:",omitempty"`
	OfList   []AssistantMessageContentPartUnion `json:",omitempty"`
}

func (u *AssistantMessageContentUnion) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		u.OfString = &s
		return nil
	}

	var list []AssistantMessageContentPartUnion
	if err := sonic.Unmarshal(data, &list); err == nil {
		u.OfList = list
		return nil
	}

	return errors.New("invalid assistant message content union")
}

func (u *AssistantMessageContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	if u.OfList != nil {
		return sonic.Marshal(u.OfList)
	}

	return nil, nil
}

type AssistantMessageFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

type AssistantMessageToolCallUnion struct {
	OfFunction AssistantMessageFunctionToolCall `json:",omitempty"`
	OfCustom   AssistantMessageCustomToolCall   `json:",omitempty"`
}

func (u *AssistantMessageToolCallUnion) UnmarshalJSON(data []byte) error {
	// First, try to unmarshal as a map to check the type field
	var raw map[string]any
	if err := sonic.Unmarshal(data, &raw); err != nil {
		return err
	}

	toolType, ok := raw["type"].(string)
	if !ok {
		return errors.New("invalid tool call: missing type field")
	}

	switch toolType {
	case "function":
		var toolCall AssistantMessageFunctionToolCall
		if err := sonic.Unmarshal(data, &toolCall); err != nil {
			return err
		}
		u.OfFunction = toolCall
		return nil
	case "custom":
		var toolCall AssistantMessageCustomToolCall
		if err := sonic.Unmarshal(data, &toolCall); err != nil {
			return err
		}
		u.OfCustom = toolCall
		return nil
	default:
		return errors.New("invalid tool call: unknown type")
	}
}

func (u *AssistantMessageToolCallUnion) MarshalJSON() ([]byte, error) {
	if u.OfFunction.Type != "" {
		return sonic.Marshal(u.OfFunction)
	}

	if u.OfCustom.Type != "" {
		return sonic.Marshal(u.OfCustom)
	}

	return nil, nil
}

type AssistantMessageFunctionToolCall struct {
	Type     string                                `json:"type"` // "function"
	ID       string                                `json:"id,omitempty"`
	Function AssistantMessageFunctionToolCallParam `json:"function"`
}

type AssistantMessageFunctionToolCallParam struct {
	Name      string `json:"name"`
	Arguments string `json:"input"`
}

type AssistantMessageCustomToolCall struct {
	Type   string                              `json:"type"` // "custom"
	ID     string                              `json:"id,omitempty"`
	Custom AssistantMessageCustomToolCallParam `json:"custom"`
}

type AssistantMessageCustomToolCallParam struct {
	Name  string `json:"name"`
	Input string `json:"input"`
}

type ToolMessageContentUnion struct {
	OfString *string    `json:",omitempty"`
	OfList   []TextPart `json:",omitempty"`
}

func (u *ToolMessageContentUnion) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		u.OfString = &s
		return nil
	}

	var list []TextPart
	if err := sonic.Unmarshal(data, &list); err == nil {
		u.OfList = list
		return nil
	}

	return errors.New("invalid tool message content union")
}

func (u *ToolMessageContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	if u.OfList != nil {
		return sonic.Marshal(u.OfList)
	}

	return nil, nil
}

type AssistantMessageContentPartUnion struct {
	OfText    *TextPart    `json:",omitempty"`
	OfRefusal *RefusalPart `json:",omitempty"`
}

func (u *AssistantMessageContentPartUnion) UnmarshalJSON(data []byte) error {
	// First, try to unmarshal as a map to check the type field
	var raw map[string]any
	if err := sonic.Unmarshal(data, &raw); err != nil {
		return err
	}

	partType, ok := raw["type"].(string)
	if !ok {
		return errors.New("invalid assistant message content part: missing type field")
	}

	switch partType {
	case "text":
		var part TextPart
		if err := sonic.Unmarshal(data, &part); err != nil {
			return err
		}
		u.OfText = &part
		return nil
	case "refusal":
		var part RefusalPart
		if err := sonic.Unmarshal(data, &part); err != nil {
			return err
		}
		u.OfRefusal = &part
		return nil
	default:
		return errors.New("invalid assistant message content part: unknown type")
	}
}

func (u *AssistantMessageContentPartUnion) MarshalJSON() ([]byte, error) {
	if u.OfText != nil {
		return sonic.Marshal(u.OfText)
	}

	if u.OfRefusal != nil {
		return sonic.Marshal(u.OfRefusal)
	}

	return nil, nil
}

type UserMessageContentPartUnion struct {
	OfText       *TextPart  `json:",omitempty"`
	OfImageUrl   *ImagePart `json:",omitempty"`
	OfInputAudio *AudioPart `json:",omitempty"`
	OfFile       *FilePart  `json:",omitempty"`
}

func (u *UserMessageContentPartUnion) UnmarshalJSON(data []byte) error {
	// First, try to unmarshal as a map to check the type field
	var raw map[string]any
	if err := sonic.Unmarshal(data, &raw); err != nil {
		return err
	}

	partType, ok := raw["type"].(string)
	if !ok {
		return errors.New("invalid user message content part: missing type field")
	}

	switch partType {
	case "text":
		var part TextPart
		if err := sonic.Unmarshal(data, &part); err != nil {
			return err
		}
		u.OfText = &part
		return nil
	case "image_url":
		var part ImagePart
		if err := sonic.Unmarshal(data, &part); err != nil {
			return err
		}
		u.OfImageUrl = &part
		return nil
	case "input_audio":
		var part AudioPart
		if err := sonic.Unmarshal(data, &part); err != nil {
			return err
		}
		u.OfInputAudio = &part
		return nil
	case "file":
		var part FilePart
		if err := sonic.Unmarshal(data, &part); err != nil {
			return err
		}
		u.OfFile = &part
		return nil
	default:
		return errors.New("invalid user message content part: unknown type")
	}
}

func (u *UserMessageContentPartUnion) MarshalJSON() ([]byte, error) {
	if u.OfText != nil {
		return sonic.Marshal(u.OfText)
	}

	if u.OfImageUrl != nil {
		return sonic.Marshal(u.OfImageUrl)
	}

	if u.OfInputAudio != nil {
		return sonic.Marshal(u.OfInputAudio)
	}

	if u.OfFile != nil {
		return sonic.Marshal(u.OfFile)
	}

	return nil, nil
}

type RefusalPart struct {
	Type    string `json:"type"` // "refusal"
	Refusal string `json:"refusal,omitempty"`
}

type TextPart struct {
	Type string `json:"type,omitempty"` // "text"
	Text string `json:"text,omitempty"`
}

type ImagePart struct {
	Type     string   `json:"type,omitempty"` // "image_url"
	ImageUrl ImageUrl `json:"image_url,omitempty"`
}

type AudioPart struct {
	Type       string     `json:"type,omitempty"` // "input_audio"
	InputAudio InputAudio `json:"input_audio,omitempty"`
}

type FilePart struct {
	Type string `json:"type,omitempty"`
	File File   `json:"file,omitempty"`
}

type ImageUrl struct {
	Url    string `json:"url"`
	Detail string `json:"detail"` // "auto", "low", "high"
}

type InputAudio struct {
	Format string `json:"format,omitzero"` // "wav", "mp3"
	Data   string `json:"data"`
}

type File struct {
	FileID   *string `json:"file_id,omitempty"`
	Filename *string `json:"filename,omitempty"`
	FileData *string `json:"file_data,omitempty"`
}
