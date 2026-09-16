package restate_runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

// capturingProvider records the request the step hands it and answers with an
// empty, completed stream. The rest of llm.Provider is the nil embedded
// interface, which panics if reached — the assertion we want.
type capturingProvider struct {
	llm.Provider
	seen *responses.Request
}

type generatedImageProvider struct {
	llm.Provider
	result string
	chunks []*responses.ResponseChunk
}

func (p *generatedImageProvider) NewStreamingResponses(context.Context, *responses.Request) (chan *responses.ResponseChunk, error) {
	p.chunks = []*responses.ResponseChunk{
		{OfImageGenerationCallPartialImage: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{ItemId: "ig_restate", PartialImageBase64: "preview"}},
		{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{Item: responses.ChunkOutputItemData{
			Type: "image_generation_call", Id: "ig_restate", Status: "completed",
			OutputFormat: utils.Ptr("png"), Result: &p.result,
		}}},
		{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{Response: responses.ChunkResponseData{Output: []responses.OutputMessageUnion{{
			OfImageGenerationCall: &responses.ImageGenerationCallMessage{ID: "ig_restate", Status: "completed", OutputFormat: "png", Result: p.result},
		}}}}},
	}
	stream := make(chan *responses.ResponseChunk, len(p.chunks))
	for _, chunk := range p.chunks {
		stream <- chunk
	}
	close(stream)
	return stream, nil
}

type countingStore struct {
	attachments.UploadStore
	puts int
}

func (s *countingStore) Put(ctx context.Context, namespace string, upload attachments.Upload) (attachments.Ref, error) {
	s.puts++
	return s.UploadStore.Put(ctx, namespace, upload)
}

func (p *capturingProvider) NewStreamingResponses(_ context.Context, in *responses.Request) (chan *responses.ResponseChunk, error) {
	p.seen = in
	stream := make(chan *responses.ResponseChunk, 1)
	stream <- &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{}}
	close(stream)
	return stream, nil
}

// The LLM step's body is where references become bytes, under the run's
// namespace: the request arrives as history keeps it, the provider is handed
// the bytes, and a step under another namespace cannot read them at all.
func TestRestateLLMStepResolvesReferencesUnderTheTrustedScope(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII=")
	require.NoError(t, err)
	ref, err := store.Put(t.Context(), "tenant", attachments.Upload{Filename: "pixel.png", MediaType: "image/png", Content: bytes.NewReader(png)})
	require.NoError(t, err)
	request := &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{{
		OfInputMessage: &responses.InputMessage{Role: constants.RoleUser, Content: responses.InputContent{
			{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(attachments.FileID(ref))}},
		}},
	}}}}
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})

	provider := &capturingProvider{}
	step := NewRestateLLM(nil, provider, "", nil, "stream", middleware).(*RestateLLM)
	// The namespace comes from the call the loop hands the step.
	_, err = step.invoke(context.Background(), &agents.ModelCall{Namespace: "tenant"}, request, func(*responses.ResponseChunk) {})
	require.NoError(t, err)

	sent, err := json.Marshal(provider.seen)
	require.NoError(t, err)
	require.Contains(t, string(sent), "data:image/png;base64,")
	require.NotContains(t, string(sent), "attachment://")

	kept, err := json.Marshal(request)
	require.NoError(t, err)
	require.Contains(t, string(kept), "attachment://")
	require.NotContains(t, string(kept), "base64", "the request the handler journals carries no bytes")

	other := &capturingProvider{}
	stranger := NewRestateLLM(nil, other, "", nil, "stream", middleware).(*RestateLLM)
	_, err = stranger.invoke(context.Background(), &agents.ModelCall{Namespace: "other-tenant"}, request, func(*responses.ResponseChunk) {})
	require.Error(t, err)
	require.Nil(t, other.seen, "the provider is never contacted for a reference the step may not read")
}

// invoke is the Restate step body. Its serialized return must contain only an
// owned reference; raw generated bytes remain confined to the step.
func TestRestateLLMStepReturnsOnlyGeneratedImageReference(t *testing.T) {
	base, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer base.Close()
	store := &countingStore{UploadStore: base}
	raw := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII="
	provider := &generatedImageProvider{result: raw}
	middleware := agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})
	step := NewRestateLLM(nil, provider, "", nil, "stream", middleware).(*RestateLLM)

	var published []*responses.ResponseChunk
	got, err := step.invoke(context.Background(), &agents.ModelCall{Namespace: "tenant"}, &responses.Request{}, func(chunk *responses.ResponseChunk) {
		published = append(published, chunk)
	})
	require.NoError(t, err)
	require.Equal(t, 1, store.puts, "stream and accumulated step output reuse one upload")
	require.Len(t, got.Output, 1)
	result := got.Output[0].OfImageGenerationCall.Result
	require.True(t, attachments.IsFileID(result))
	journaled, err := json.Marshal(got)
	require.NoError(t, err)
	require.Contains(t, string(journaled), "attachment://")
	require.NotContains(t, string(journaled), raw)
	require.Len(t, published, 2, "the binary preview is not published")
	require.Equal(t, result, *published[0].OfOutputItemDone.Item.Result)
	require.Equal(t, result, published[1].OfResponseCompleted.Response.Output[0].OfImageGenerationCall.Result)
	for _, chunk := range published {
		wire, marshalErr := json.Marshal(chunk)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(wire), raw)
		require.NotContains(t, string(wire), "preview")
	}
	require.Equal(t, raw, *provider.chunks[1].OfOutputItemDone.Item.Result, "provider chunks remain unchanged")
	require.Equal(t, raw, provider.chunks[2].OfResponseCompleted.Response.Output[0].OfImageGenerationCall.Result)

	ref, err := attachments.RefFromFileID(result)
	require.NoError(t, err)
	blob, err := attachments.NewResolver(base, attachments.Config{}).Resolve(t.Context(), "tenant", ref)
	require.NoError(t, err)
	var persisted bytes.Buffer
	_, err = blob.WriteTo(&persisted)
	require.NoError(t, err)
	want, err := base64.StdEncoding.DecodeString(raw)
	require.NoError(t, err)
	require.Equal(t, want, persisted.Bytes())
}
