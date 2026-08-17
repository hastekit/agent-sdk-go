package chat_completion

import (
	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
)

type ResponseChunk struct {
	OfChatCompletionChunk *ChatCompletionChunk `json:",omitempty"`
}

func (u *ResponseChunk) UnmarshalJSON(data []byte) error {
	var chatCompletionChunk *ChatCompletionChunk
	if err := sonic.Unmarshal(data, &chatCompletionChunk); err == nil {
		u.OfChatCompletionChunk = chatCompletionChunk
		return err
	}
	return nil
}

func (u *ResponseChunk) MarshalJSON() ([]byte, error) {
	return sonic.Marshal(u.OfChatCompletionChunk)
}

type ChatCompletionChunk struct {
	Id                string                      `json:"id"`
	Object            string                      `json:"object"`
	Created           int                         `json:"created"`
	Model             string                      `json:"model"`
	ServiceTier       string                      `json:"service_tier"`
	SystemFingerprint string                      `json:"system_fingerprint"`
	Choices           []ChatCompletionChunkChoice `json:"choices"`
	Obfuscation       string                      `json:"obfuscation"`
	Usage             *Usage                      `json:"usage,omitempty"`
}

type ChatCompletionChunkChoice struct {
	Index        int                            `json:"index"`
	Delta        ChatCompletionChunkChoiceDelta `json:"delta"`
	FinishReason string                         `json:"finish_reason"`
	Obfuscation  *string                        `json:"obfuscation,omitempty"`
}

type ChatCompletionChunkChoiceDelta struct {
	Role    constants.Role `json:"role"`
	Content string         `json:"content"`
	Refusal string         `json:"refusal"`
}
