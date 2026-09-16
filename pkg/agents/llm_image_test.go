package agents

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
)

func imageOutput(id, result, format string) responses.OutputMessageUnion {
	return responses.OutputMessageUnion{OfImageGenerationCall: &responses.ImageGenerationCallMessage{
		ID: id, Status: "completed", OutputFormat: format, Result: result,
	}}
}

func completedChunk(output ...responses.OutputMessageUnion) *responses.ResponseChunk {
	return &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{
		Response: responses.ChunkResponseData{Output: output},
	}}
}

// Partial image events are successive previews, not base64 fragments. The
// accumulator keeps only the final item and reconciles output_item.done with
// the authoritative response.completed output without duplicating it.
func TestReadStreamKeepsCompleteGeneratedImageWithoutJoiningPreviews(t *testing.T) {
	stream := make(chan *responses.ResponseChunk, 4)
	stream <- &responses.ResponseChunk{OfImageGenerationCallPartialImage: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{
		ItemId: "ig_1", PartialImageIndex: 0, PartialImageBase64: "preview-one",
	}}
	stream <- &responses.ResponseChunk{OfImageGenerationCallPartialImage: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{
		ItemId: "ig_1", PartialImageIndex: 1, PartialImageBase64: "preview-two",
	}}
	stream <- &responses.ResponseChunk{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
		Item: responses.ChunkOutputItemData{
			Type: "image_generation_call", Id: "ig_1", Status: "completed",
			OutputFormat: utils.Ptr("png"), Quality: utils.Ptr("high"), Result: utils.Ptr("done-image"),
		},
	}}
	// Some providers repeat the item in response.completed. Its non-empty
	// final result wins while metadata absent there is retained from done.
	stream <- completedChunk(imageOutput("ig_1", "completed-image", ""))
	close(stream)

	var published []*responses.ResponseChunk
	got, err := (&Accumulator{}).ReadStream(context.Background(), stream, func(chunk *responses.ResponseChunk) {
		published = append(published, chunk)
	})
	require.NoError(t, err)
	require.Len(t, published, 4, "raw preview chunks remain available to streaming consumers")
	require.Len(t, got.Output, 1)
	image := got.Output[0].OfImageGenerationCall
	require.NotNil(t, image)
	require.Equal(t, "completed-image", image.Result)
	require.Equal(t, "png", image.OutputFormat)
	require.Equal(t, "high", image.Quality)
	require.NotContains(t, image.Result, "preview-one")
	require.NotContains(t, image.Result, "preview-two")
}

// Providers may omit optional image metadata on output_item.done, or may put
// the complete output only on response.completed. Neither form may panic or
// lose the final image.
func TestReadStreamGeneratedImageOptionalMetadataAndCompletedOnly(t *testing.T) {
	t.Run("nil optional metadata", func(t *testing.T) {
		stream := make(chan *responses.ResponseChunk, 2)
		stream <- &responses.ResponseChunk{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Item: responses.ChunkOutputItemData{Type: "image_generation_call", Id: "ig_nil", Result: utils.Ptr("final")},
		}}
		stream <- completedChunk()
		close(stream)
		got, err := (&Accumulator{}).ReadStream(context.Background(), stream, func(*responses.ResponseChunk) {})
		require.NoError(t, err)
		require.Len(t, got.Output, 1)
		require.Equal(t, "final", got.Output[0].OfImageGenerationCall.Result)
		require.Empty(t, got.Output[0].OfImageGenerationCall.OutputFormat)
	})

	t.Run("completed only", func(t *testing.T) {
		stream := make(chan *responses.ResponseChunk, 1)
		stream <- completedChunk(imageOutput("ig_completed", "final", "png"))
		close(stream)
		got, err := (&Accumulator{}).ReadStream(context.Background(), stream, func(*responses.ResponseChunk) {})
		require.NoError(t, err)
		require.Len(t, got.Output, 1)
		require.Equal(t, "ig_completed", got.Output[0].OfImageGenerationCall.ID)
		require.Equal(t, "final", got.Output[0].OfImageGenerationCall.Result)
	})
}
