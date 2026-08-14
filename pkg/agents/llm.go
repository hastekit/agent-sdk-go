package agents

import (
	"context"

	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
)

type LLM interface {
	NewStreamingResponses(ctx context.Context, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error)
}

type WrappedLLM struct {
	llm llm.Provider
}

func (l *WrappedLLM) NewStreamingResponses(ctx context.Context, in *responses.Request, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	acc := Accumulator{}

	stream, err := l.llm.NewStreamingResponses(ctx, in)
	if err != nil {
		return nil, err
	}

	return acc.ReadStream(ctx, stream, cb)
}

type Accumulator struct {
}

// ReadStream folds a provider's chunk stream into one response, publishing
// each chunk as it arrives.
//
// It gives up when ctx is cancelled — which is how a stop ends a model call
// mid-stream — and reports ErrModelCallStopped. Whatever had accumulated is
// dropped rather than returned: a stopped run does not answer, and the loop
// writes its cancellation notice instead. The unread remainder is drained in
// the background so the provider's sender is never left blocked on a channel
// nobody is reading.
func (a *Accumulator) ReadStream(ctx context.Context, stream chan *responses.ResponseChunk, cb func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	// Process stream
	finalOutput := []responses.OutputMessageUnion{}
	var usage *responses.Usage
	for {
		var chunk *responses.ResponseChunk
		var open bool

		select {
		case chunk, open = <-stream:
			if !open {
				// A provider that honours cancellation closes its stream, so
				// the end of the channel is ambiguous: it means either the
				// model finished or the stop reached it first. The context is
				// what tells them apart — without this check the outcome would
				// depend on which of the two raced ahead, and a stopped run
				// would sometimes be recorded as a complete answer.
				if ctx.Err() != nil {
					return nil, ErrModelCallStopped
				}
				return &responses.Response{Output: finalOutput, Usage: usage}, nil
			}
		case <-ctx.Done():
			go drain(stream)
			return nil, ErrModelCallStopped
		}

		cb(chunk)
		switch chunk.ChunkType() {
		case "response.output_item.done":
			if chunk.OfOutputItemDone.Item.Type == "message" {
				for _, content := range *chunk.OfOutputItemDone.Item.Content {
					if content.OfOutputText != nil {
						finalOutput = append(finalOutput, responses.OutputMessageUnion{
							OfOutputMessage: &responses.OutputMessage{
								ID:   chunk.OfOutputItemDone.Item.Id,
								Role: constants.RoleAssistant,
								Content: &responses.OutputContent{
									{OfOutputText: content.OfOutputText},
								},
							},
						})
					}
				}
			}

			if chunk.OfOutputItemDone.Item.Type == "reasoning" {
				// reasoning_text (only OSS)
				// We unify `reasoning_text` into `summary_text` for simplicity
				if chunk.OfOutputItemDone.Item.Content != nil {
					for _, content := range *chunk.OfOutputItemDone.Item.Content {
						if content.OfReasoningText != nil {
							finalOutput = append(finalOutput, responses.OutputMessageUnion{
								OfReasoning: &responses.ReasoningMessage{
									ID: chunk.OfOutputItemDone.Item.Id,
									Summary: []responses.SummaryTextContent{
										{
											Text: content.OfReasoningText.Text,
										},
									},
								},
							})
						}
					}
				}

				// Skip empty reasoning blocks
				if chunk.OfOutputItemDone.Item.EncryptedContent == nil && len(*chunk.OfOutputItemDone.Item.Summary) == 0 {
					continue
				}

				var encryptedContent *string
				if chunk.OfOutputItemDone.Item.EncryptedContent != nil {
					encryptedContent = chunk.OfOutputItemDone.Item.EncryptedContent
				}

				finalOutput = append(finalOutput, responses.OutputMessageUnion{
					OfReasoning: &responses.ReasoningMessage{
						ID:               chunk.OfOutputItemDone.Item.Id,
						Summary:          *chunk.OfOutputItemDone.Item.Summary,
						EncryptedContent: encryptedContent,
					},
				})
			}

			if chunk.OfOutputItemDone.Item.Type == "function_call" {
				finalOutput = append(finalOutput, responses.OutputMessageUnion{
					OfFunctionCall: &responses.FunctionCallMessage{
						ID:               chunk.OfOutputItemDone.Item.Id,
						CallID:           *chunk.OfOutputItemDone.Item.CallID,
						Name:             *chunk.OfOutputItemDone.Item.Name,
						Arguments:        *chunk.OfOutputItemDone.Item.Arguments,
						ThoughtSignature: chunk.OfOutputItemDone.Item.ThoughtSignature,
					},
				})
			}

			if chunk.OfOutputItemDone.Item.Type == "image_generation_call" {
				finalOutput = append(finalOutput, responses.OutputMessageUnion{
					OfImageGenerationCall: &responses.ImageGenerationCallMessage{
						ID:           chunk.OfOutputItemDone.Item.Id,
						Status:       chunk.OfOutputItemDone.Item.Status,
						Background:   *chunk.OfOutputItemDone.Item.Background,
						OutputFormat: *chunk.OfOutputItemDone.Item.OutputFormat,
						Quality:      *chunk.OfOutputItemDone.Item.Quality,
						Size:         *chunk.OfOutputItemDone.Item.Size,
						Result:       *chunk.OfOutputItemDone.Item.Result,
					},
				})
			}

		case "response.completed":
			usage = &chunk.OfResponseCompleted.Response.Usage
		}
	}
}

// drain reads a stream to its end and discards it, so a provider still writing
// into a channel nobody reads is released rather than blocked forever.
func drain(stream chan *responses.ResponseChunk) {
	for range stream {
	}
}
