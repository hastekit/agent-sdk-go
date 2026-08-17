package base

import (
	"context"
	"net/http"

	chat_completion2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	embeddings2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	image_edit2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	image_generation2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	speech2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	transcription2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
)

type BaseProvider struct{}

func (bp *BaseProvider) NewResponses(ctx context.Context, in *responses2.Request) (*responses2.Response, error) {
	panic("implement me")
}

func (bp *BaseProvider) NewStreamingResponses(ctx context.Context, in *responses2.Request) (chan *responses2.ResponseChunk, error) {
	panic("implement me")
}

func (bp *BaseProvider) NewEmbedding(ctx context.Context, in *embeddings2.Request) (*embeddings2.Response, error) {
	panic("implement me")
}

func (bp *BaseProvider) NewChatCompletion(ctx context.Context, in *chat_completion2.Request) (*chat_completion2.Response, error) {
	panic("implement me")
}

func (bp *BaseProvider) NewStreamingChatCompletion(ctx context.Context, in *chat_completion2.Request) (chan *chat_completion2.ResponseChunk, error) {
	panic("implement me")
}

func (bp *BaseProvider) NewSpeech(ctx context.Context, in *speech2.Request) (*speech2.Response, error) {
	return nil, nil
}

func (bp *BaseProvider) NewStreamingSpeech(ctx context.Context, in *speech2.Request) (chan *speech2.ResponseChunk, error) {
	return nil, nil
}

func (bp *BaseProvider) NewTranscription(ctx context.Context, in *transcription2.Request) (*transcription2.Response, error) {
	return nil, nil
}

func (bp *BaseProvider) NewImageGeneration(ctx context.Context, in *image_generation2.Request) (*image_generation2.Response, error) {
	return nil, nil
}

func (bp *BaseProvider) NewImageEdit(ctx context.Context, in *image_edit2.Request) (*image_edit2.Response, error) {
	return nil, nil
}

func AddAdditionalHeaders(req *http.Request, extraFields map[string]any) {
	if extraFields != nil {
		if additionalHeaders, ok := extraFields["additional_headers"]; ok {
			for k, v := range additionalHeaders.(map[string]any) {
				if vv, ok := v.(string); ok {
					req.Header.Set(k, vv)
				}
			}
		}
	}
}
