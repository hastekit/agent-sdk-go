package temporal_runtime_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// capturingProvider records the request the activity hands it and answers
// with an empty, completed stream. The rest of llm.Provider is the nil
// embedded interface, which panics if reached — the assertion we want.
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
		{OfImageGenerationCallPartialImage: &responses.ChunkImageGenerationCall[constants.ChunkTypeImageGenerationCallPartialImage]{ItemId: "ig_temporal", PartialImageBase64: "preview"}},
		{OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{
			Item: responses.ChunkOutputItemData{
				Type: "image_generation_call", Id: "ig_temporal", Status: "completed",
				OutputFormat: utils.Ptr("png"), Result: &p.result,
			},
		}},
		{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{Response: responses.ChunkResponseData{Output: []responses.OutputMessageUnion{{
			OfImageGenerationCall: &responses.ImageGenerationCallMessage{ID: "ig_temporal", Status: "completed", OutputFormat: "png", Result: p.result},
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

type recordingBroker struct {
	agents.StreamBroker
	chunks []*responses.ResponseChunk
}

func (b *recordingBroker) Publish(ctx context.Context, channel string, chunk *responses.ResponseChunk) error {
	b.chunks = append(b.chunks, chunk)
	return b.StreamBroker.Publish(ctx, channel, chunk)
}

func (p *capturingProvider) NewStreamingResponses(_ context.Context, in *responses.Request) (chan *responses.ResponseChunk, error) {
	p.seen = in
	stream := make(chan *responses.ResponseChunk, 1)
	stream <- &responses.ResponseChunk{OfResponseCompleted: &responses.ChunkResponse[constants.ChunkTypeResponseCompleted]{}}
	close(stream)
	return stream, nil
}

// activityRequestTransform asserts the wrap runs inside an activity, not in
// workflow code.
type activityRequestTransform struct {
	*agentmiddleware.AttachmentMiddleware
	t *testing.T
}

func (h *activityRequestTransform) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc {
	wrapped := h.AttachmentMiddleware.WrapModelCall(next)
	return func(ctx context.Context, call *agents.ModelCall, in *responses.Request) (*responses.Response, error) {
		require.True(h.t, activity.IsActivity(ctx), "the wrap must run in an activity, not workflow code")
		return wrapped(ctx, call, in)
	}
}

// referencedRequest is a request as history keeps it: a user message carrying
// a attachment file_id to an image in store, and no bytes.
func referencedRequest(t *testing.T, store attachments.UploadStore) *responses.Request {
	t.Helper()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII=")
	require.NoError(t, err)
	ref, err := store.Put(t.Context(), "test", attachments.Upload{Filename: "pixel.png", MediaType: "image/png", Content: bytes.NewReader(png)})
	require.NoError(t, err)
	return &responses.Request{Input: responses.InputUnion{OfInputMessageList: []responses.InputMessageUnion{{
		OfInputMessage: &responses.InputMessage{Role: constants.RoleUser, Content: responses.InputContent{
			{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(attachments.FileID(ref))}},
		}},
	}}}}
}

// The LLM activity is where references become bytes: the request crosses into
// it as history keeps it with its call alongside, the provider is handed the
// bytes read under the call's namespace, and the journal sees neither the
// bytes nor a wrap activity of its own.
func TestTemporalLLMActivityResolvesReferencesInsideTheActivity(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	provider := &capturingProvider{}
	middleware := &activityRequestTransform{AttachmentMiddleware: agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true, Store: store}), t: t}

	a := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:        "A",
		LLM:         provider,
		History:     history.NewConversationManager(history.NewInMemoryConversationPersistence()),
		Middlewares: []agents.Middleware{middleware},
	}, streambroker.NewMemoryStreamBroker())
	activities := a.GetActivities()
	for name := range activities {
		require.NotContains(t, name, "WrapModelCall", "the wrap must not become an activity of its own")
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	fn := activities["A_NewStreamingResponsesActivity"]
	env.RegisterActivity(fn)

	request := referencedRequest(t, store)
	journaled, err := json.Marshal(request)
	require.NoError(t, err)
	require.Contains(t, string(journaled), "attachment://")
	require.NotContains(t, string(journaled), "base64", "what the workflow schedules carries no bytes")

	_, err = env.ExecuteActivity(fn, request, &agents.ModelCall{AgentName: "A", Namespace: "test"})
	require.NoError(t, err)

	require.NotNil(t, provider.seen)
	sent, err := json.Marshal(provider.seen)
	require.NoError(t, err)
	require.Contains(t, string(sent), "data:image/png;base64,")
	require.NotContains(t, string(sent), "attachment://")
}

// The activity's return value is what Temporal serializes into workflow
// history, so generated bytes must already be replaced there.
func TestTemporalLLMActivityReturnsOnlyGeneratedImageReference(t *testing.T) {
	base, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer base.Close()
	store := &countingStore{UploadStore: base}
	raw := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6xkAAAAASUVORK5CYII="
	provider := &generatedImageProvider{result: raw}
	broker := &recordingBroker{StreamBroker: streambroker.NewMemoryStreamBroker()}
	a := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name: "image-agent", LLM: provider,
		History:     history.NewConversationManager(history.NewInMemoryConversationPersistence()),
		Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})},
	}, broker)
	fn := a.GetActivities()["image-agent_NewStreamingResponsesActivity"]

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(fn)
	value, err := env.ExecuteActivity(fn, &responses.Request{}, &agents.ModelCall{AgentName: "image-agent", Namespace: "tenant"})
	require.NoError(t, err)
	var got responses.Response
	require.NoError(t, value.Get(&got))
	require.Equal(t, 1, store.puts, "stream and accumulated activity output reuse one upload")
	require.Len(t, got.Output, 1)
	result := got.Output[0].OfImageGenerationCall.Result
	require.True(t, attachments.IsFileID(result))
	journaled, err := json.Marshal(got)
	require.NoError(t, err)
	require.Contains(t, string(journaled), "attachment://")
	require.NotContains(t, string(journaled), raw)
	require.Len(t, broker.chunks, 2, "the binary preview is not published")
	require.Equal(t, result, *broker.chunks[0].OfOutputItemDone.Item.Result)
	require.Equal(t, result, broker.chunks[1].OfResponseCompleted.Response.Output[0].OfImageGenerationCall.Result)
	for _, chunk := range broker.chunks {
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
