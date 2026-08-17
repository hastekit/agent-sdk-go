package responses

import (
	"errors"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
)

type Response struct {
	ID          string                 `json:"id"`
	Model       string                 `json:"model"`
	Output      []OutputMessageUnion   `json:"output"`
	Usage       *Usage                 `json:"usage"`
	Error       *Error                 `json:"error"`
	ServiceTier string                 `json:"service_tier"`
	Metadata    map[string]interface{} `json:"metadata"`
}

type Error struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Param   string `json:"param"`
	Code    string `json:"code"`
}

// OutputMessageUnion represents all possible message outputs from the model.
// Model can output: "text", "function_call", "reasoning" or "image_generation_call"
type OutputMessageUnion struct {
	OfOutputMessage       *OutputMessage              `json:",omitempty"`
	OfFunctionCall        *FunctionCallMessage        `json:",omitempty"`
	OfReasoning           *ReasoningMessage           `json:",omitempty"`
	OfImageGenerationCall *ImageGenerationCallMessage `json:",omitempty"`
	OfWebSearchCall       *WebSearchCallMessage       `json:",omitempty"`
	OfCodeInterpreterCall *CodeInterpreterCallMessage `json:",omitempty"`
}

func (u *OutputMessageUnion) UnmarshalJSON(data []byte) error {
	var outputMessage *OutputMessage
	if err := sonic.Unmarshal(data, &outputMessage); err == nil {
		u.OfOutputMessage = outputMessage
		return nil
	}

	var fnCallMessage *FunctionCallMessage
	if err := sonic.Unmarshal(data, &fnCallMessage); err == nil {
		u.OfFunctionCall = fnCallMessage
		return nil
	}

	var reasoningMessage *ReasoningMessage
	if err := sonic.Unmarshal(data, &reasoningMessage); err == nil {
		u.OfReasoning = reasoningMessage
		return nil
	}

	var imageGenerationCall *ImageGenerationCallMessage
	if err := sonic.Unmarshal(data, &imageGenerationCall); err == nil {
		u.OfImageGenerationCall = imageGenerationCall
		return nil
	}

	var webSearchCallMessage *WebSearchCallMessage
	if err := sonic.Unmarshal(data, &webSearchCallMessage); err == nil {
		u.OfWebSearchCall = webSearchCallMessage
		return nil
	}

	var codeInterpreterCallMessage *CodeInterpreterCallMessage
	if err := sonic.Unmarshal(data, &codeInterpreterCallMessage); err == nil {
		u.OfCodeInterpreterCall = codeInterpreterCallMessage
		return nil
	}

	return errors.New("invalid output message union type")
}

func (u *OutputMessageUnion) MarshalJSON() ([]byte, error) {
	if u.OfOutputMessage != nil {
		return sonic.Marshal(u.OfOutputMessage)
	}

	if u.OfFunctionCall != nil {
		return sonic.Marshal(u.OfFunctionCall)
	}

	if u.OfReasoning != nil {
		return sonic.Marshal(u.OfReasoning)
	}

	if u.OfImageGenerationCall != nil {
		return sonic.Marshal(u.OfImageGenerationCall)
	}

	if u.OfWebSearchCall != nil {
		return sonic.Marshal(u.OfWebSearchCall)
	}

	if u.OfCodeInterpreterCall != nil {
		return sonic.Marshal(u.OfCodeInterpreterCall)
	}

	return nil, nil
}

func (u *OutputMessageUnion) AsInput() (InputMessageUnion, error) {
	if u.OfOutputMessage != nil {
		return InputMessageUnion{OfOutputMessage: u.OfOutputMessage}, nil
	}

	if u.OfReasoning != nil {
		return InputMessageUnion{OfReasoning: u.OfReasoning}, nil
	}

	if u.OfFunctionCall != nil {
		return InputMessageUnion{OfFunctionCall: u.OfFunctionCall}, nil
	}

	if u.OfImageGenerationCall != nil {
		return InputMessageUnion{OfImageGenerationCall: u.OfImageGenerationCall}, nil
	}

	if u.OfWebSearchCall != nil {
		return InputMessageUnion{OfWebSearchCall: u.OfWebSearchCall}, nil
	}

	if u.OfCodeInterpreterCall != nil {
		return InputMessageUnion{OfCodeInterpreterCall: u.OfCodeInterpreterCall}, nil
	}

	return InputMessageUnion{}, errors.New("invalid output message union type")
}

type OutputMessage struct {
	ID      string                       `json:"id"`
	Type    constants.MessageTypeMessage `json:"type,omitempty"`          // Always "message".
	Role    constants.Role               `json:"role,omitempty,required"` // Any of "user", "system", "developer".
	Content *OutputContent               `json:"content,omitempty,required"`
}

type OutputContent []OutputContentUnion

type OutputContentUnion struct {
	OfOutputText *OutputTextContent `json:",omitempty,inline"`
}

func (u *OutputContentUnion) UnmarshalJSON(data []byte) error {
	var textContext OutputTextContent
	if err := sonic.Unmarshal(data, &textContext); err == nil {
		u.OfOutputText = &textContext
		return nil
	}

	return errors.New("invalid input content union")
}

func (u *OutputContentUnion) MarshalJSON() ([]byte, error) {
	if u.OfOutputText != nil {
		return sonic.Marshal(u.OfOutputText)
	}

	return nil, nil
}

// Usage follows OpenAI's token accounting, since the OpenAI and xAI providers
// decode straight onto this struct. Every provider must normalize to it:
//
//   - InputTokens is the *entire* prompt, cached portions included.
//   - InputTokensDetails.CachedTokens is a *subset* of InputTokens — the part
//     served from a prompt cache. It is never additional to it.
//   - TotalTokens == InputTokens + OutputTokens.
//
// The subset rule matters: callers gauge how full the context window is from
// InputTokens (see the history summarizer's trigger). A provider that reports
// only the uncached remainder there makes a nearly-full context look empty
// whenever prompt caching is on.
type Usage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

func NewOutputItemMessageID() string {
	return "msg_" + uuid.NewString()
}

func NewOutputItemFunctionCallID() string {
	return "fc_" + uuid.NewString()
}

func NewOutputItemReasoningID() string {
	return "rs_" + uuid.NewString()
}

func NewOutputItemWebSearchCallID() string {
	return "ws_" + uuid.NewString()
}
