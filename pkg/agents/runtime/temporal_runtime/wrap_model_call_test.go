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
	middleware := &activityRequestTransform{AttachmentMiddleware: agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store}), t: t}

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
