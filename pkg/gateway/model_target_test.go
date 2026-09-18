package gateway

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

type targetAdapter struct {
	LLMGatewayAdapter
	provider   llm.ProviderName
	key, model string
}

func (a *targetAdapter) NewStreamingResponses(_ context.Context, p llm.ProviderName, key string, r *responses.Request) (chan *responses.ResponseChunk, error) {
	a.provider, a.key, a.model = p, key, r.Model
	ch := make(chan *responses.ResponseChunk)
	close(ch)
	return ch, nil
}
func TestExplicitModelTargetDoesNotChangeBoundModelOrReuseItsKey(t *testing.T) {
	adapter := &targetAdapter{}
	store := NewInMemoryConfigStore([]ProviderConfig{{ProviderName: llm.ProviderNameAnthropic, ApiKeys: []*APIKeyConfig{{APIKey: "other-key"}}}})
	client := NewLLMClient(adapter, store, WithModel(llm.ProviderNameOpenAI, "primary"), WithKey("primary-key"))
	request := &responses.Request{Model: "history-model"}
	_, err := client.NewStreamingResponsesForModel(t.Context(), "Anthropic/backup", request)
	require.NoError(t, err)
	require.Equal(t, llm.ProviderNameAnthropic, adapter.provider)
	require.Equal(t, "other-key", adapter.key)
	require.Equal(t, "backup", adapter.model)
	require.Equal(t, "history-model", request.Model)
	_, err = client.NewStreamingResponses(t.Context(), &responses.Request{})
	require.NoError(t, err)
	require.Equal(t, "primary", adapter.model)
	require.Equal(t, "primary-key", adapter.key)
}
