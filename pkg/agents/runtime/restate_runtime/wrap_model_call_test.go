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
