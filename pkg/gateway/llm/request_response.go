package llm

import (
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
)

type Request struct {
	OfEmbeddingsInput     *embeddings.Request
	OfResponsesInput      *responses.Request
	OfChatCompletionInput *chat_completion.Request
	OfSpeech              *speech.Request
	OfTranscription       *transcription.Request
	OfImageGeneration     *image_generation.Request
	OfImageEdit           *image_edit.Request
}

func (r *Request) GetRequestedModel() string {
	if r.OfResponsesInput != nil {
		return r.OfResponsesInput.Model
	}

	if r.OfEmbeddingsInput != nil {
		return r.OfEmbeddingsInput.Model
	}

	if r.OfChatCompletionInput != nil {
		return r.OfChatCompletionInput.Model
	}

	if r.OfSpeech != nil {
		return r.OfSpeech.Model
	}

	if r.OfTranscription != nil {
		return r.OfTranscription.Model
	}

	if r.OfImageGeneration != nil {
		return r.OfImageGeneration.Model
	}

	if r.OfImageEdit != nil {
		return r.OfImageEdit.Model
	}

	return ""
}

type Response struct {
	OfEmbeddingsOutput     *embeddings.Response
	OfResponsesOutput      *responses.Response
	OfChatCompletionOutput *chat_completion.Response
	OfSpeech               *speech.Response
	OfTranscription        *transcription.Response
	OfImageGeneration      *image_generation.Response
	OfImageEdit            *image_edit.Response
	Error                  *Error
}

type StreamingResponse struct {
	ResponsesStreamData      chan *responses.ResponseChunk
	ChatCompletionStreamData chan *chat_completion.ResponseChunk
	SpeechStreamData         chan *speech.ResponseChunk
}

type Error struct {
	Message string
}
