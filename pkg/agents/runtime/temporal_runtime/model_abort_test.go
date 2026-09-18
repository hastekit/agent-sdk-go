package temporal_runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/attachments"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// failingProvider counts how often it is reached and always fails, with a
// plain error carrying no classification of its own.
type failingProvider struct {
	llm.Provider
	calls int
}

func (p *failingProvider) NewStreamingResponses(context.Context, *responses.Request) (chan *responses.ResponseChunk, error) {
	p.calls++
	return nil, errors.New("provider unavailable")
}

func missingAttachmentRequest() *responses.Request {
	fileID := "attachment://cc2c80ea-e99f-41bb-9b5f-de9f6441b9de"
	return &responses.Request{Input: responses.InputUnion{OfInputMessageList: responses.InputMessageList{{
		OfInputMessage: &responses.InputMessage{Content: responses.InputContent{{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(fileID)}}}},
	}}}}
}

// A middleware's own failure — here a reference to a file the store does not
// have — is not going to change on the next attempt. With no RetryPolicy set,
// a retryable failure would have the activity run again without limit, and the
// run would hang on the refusal instead of ending on it.
func TestTemporalLLMActivityDoesNotRetryAMiddlewaresOwnError(t *testing.T) {
	store, err := attachments.NewFileStore(t.TempDir(), attachments.FileStoreConfig{})
	require.NoError(t, err)
	defer store.Close()
	provider := &failingProvider{}
	worker := temporal_runtime.NewTemporalLLM(provider, streambroker.NewMemoryStreamBroker(),
		agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{InlineAttachments: true, Store: store}))

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(worker.NewStreamingResponsesActivity)
	_, err = env.ExecuteActivity(worker.NewStreamingResponsesActivity, missingAttachmentRequest(), &agents.ModelCall{ThreadID: "thread", SessionID: "thread", Namespace: "test"})

	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr))
	require.True(t, appErr.NonRetryable(), "a refusal must not be retried")
	require.Equal(t, temporal_runtime.ModelCallFailedErrorType, appErr.Type())
	require.ErrorContains(t, err, attachments.ErrNotFound.Error())
	require.Zero(t, provider.calls, "the provider is never contacted for a request that cannot be prepared")
}

// The other half of the contract: the provider's own failure keeps the retry
// behaviour every activity failure has, since the next attempt may well
// succeed.
func TestTemporalLLMActivityLeavesAProviderFailureRetryable(t *testing.T) {
	provider := &failingProvider{}
	worker := temporal_runtime.NewTemporalLLM(provider, streambroker.NewMemoryStreamBroker())

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(worker.NewStreamingResponsesActivity)
	_, err := env.ExecuteActivity(worker.NewStreamingResponsesActivity, &responses.Request{}, &agents.ModelCall{})

	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr))
	require.False(t, appErr.NonRetryable())
	require.Equal(t, 1, provider.calls)
}
