package restate_runtime

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	restate "github.com/restatedev/sdk-go"
	"github.com/stretchr/testify/require"
)

func missingAttachmentRequest() *responses.Request {
	fileID := "attachment://0123456789abcdef0123456789abcdef"
	return &responses.Request{Input: responses.InputUnion{OfInputMessageList: responses.InputMessageList{{
		OfInputMessage: &responses.InputMessage{Content: responses.InputContent{{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(fileID)}}}},
	}}}}
}

// A middleware's own failure — here a reference to a file the store does not
// have — is not going to change on the next attempt. A non-terminal step
// failure has Restate retry the invocation, replaying into the same step, and
// the run would hang on the refusal instead of ending on it.
func TestRestateLLMStepDoesNotRetryAMiddlewaresOwnError(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	provider := &resilienceProvider{}
	worker := NewRestateLLM(nil, provider, "", nil, "stream",
		agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true, Store: store})).(*RestateLLM)

	_, err = worker.invoke(t.Context(), &agents.ModelCall{Namespace: "test"}, missingAttachmentRequest(), func(*responses.ResponseChunk) {})

	require.Error(t, err)
	require.True(t, restate.IsTerminalError(err), "a refusal must not be retried")
	require.EqualValues(t, ModelCallFailedErrorCode, restate.ErrorCode(err))
	require.ErrorContains(t, err, attachments.ErrNotFound.Error())
	require.Zero(t, provider.calls, "the provider is never contacted for a request that cannot be prepared")
}

// The other half of the contract: the provider's own failure stays a
// non-terminal step failure, since the next attempt may well succeed.
func TestRestateLLMStepLeavesAProviderFailureRetryable(t *testing.T) {
	provider := &resilienceProvider{}
	worker := NewRestateLLM(nil, provider, "", nil, "stream").(*RestateLLM)

	_, err := worker.invoke(context.Background(), &agents.ModelCall{}, &responses.Request{}, func(*responses.ResponseChunk) {})

	require.Error(t, err)
	require.False(t, restate.IsTerminalError(err))
	require.Equal(t, 1, provider.calls)
}
