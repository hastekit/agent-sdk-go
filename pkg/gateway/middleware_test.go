package gateway

import (
	"context"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plainMiddleware wants nothing from the gateway.
type plainMiddleware struct{}

func (plainMiddleware) HandleRequest(next RequestHandler) RequestHandler { return next }
func (plainMiddleware) HandleStreamingRequest(next StreamingRequestHandler) StreamingRequestHandler {
	return next
}

// awareMiddleware records the store it was handed.
type awareMiddleware struct {
	plainMiddleware
	store ConfigStore
}

func (m *awareMiddleware) WithConfigStore(store ConfigStore) Middleware {
	bound := *m
	if bound.store == nil {
		bound.store = store
	}
	return &bound
}

var _ ConfigStoreAware = (*awareMiddleware)(nil)

func testStore() ConfigStore {
	return NewInMemoryConfigStore([]ProviderConfig{
		{ProviderName: llm.ProviderNameOpenAI, ApiKeys: []*APIKeyConfig{{APIKey: "k"}}},
	})
}

func TestUseMiddlewareInjectsTheConfigStore(t *testing.T) {
	store := testStore()
	gw := NewLLMGateway(store)

	gw.UseMiddleware(&awareMiddleware{})

	require.Len(t, gw.middlewares, 1)
	installed, ok := gw.middlewares[0].(*awareMiddleware)
	require.True(t, ok)
	assert.Same(t, store, installed.store)
}

func TestUseMiddlewareDoesNotMutateTheCallersMiddleware(t *testing.T) {
	original := &awareMiddleware{}
	gw := NewLLMGateway(testStore())

	gw.UseMiddleware(original)

	assert.Nil(t, original.store,
		"middleware shared between gateways must not be re-pointed at one gateway's configuration")
}

func TestUseMiddlewareLeavesAnExplicitStoreAlone(t *testing.T) {
	explicit := testStore()
	gw := NewLLMGateway(testStore())

	gw.UseMiddleware(&awareMiddleware{store: explicit})

	installed := gw.middlewares[0].(*awareMiddleware)
	assert.Same(t, explicit, installed.store, "a store set by the caller wins over injection")
}

func TestUseMiddlewarePassesPlainMiddlewareThrough(t *testing.T) {
	gw := NewLLMGateway(testStore())
	m := plainMiddleware{}

	gw.UseMiddleware(m)

	require.Len(t, gw.middlewares, 1)
	assert.Equal(t, m, gw.middlewares[0])
}

func TestUseMiddlewareKeepsOrder(t *testing.T) {
	gw := NewLLMGateway(testStore())

	first, second := &awareMiddleware{}, plainMiddleware{}
	gw.UseMiddleware(first, second)

	require.Len(t, gw.middlewares, 2)
	assert.IsType(t, &awareMiddleware{}, gw.middlewares[0], "outermost first")
	assert.Equal(t, second, gw.middlewares[1])
}

// The chain is built outermost-first, which is the contract WithMiddleware
// documents and the reason fallback must be listed before retry.
func TestMiddlewareChainRunsOutermostFirst(t *testing.T) {
	var order []string

	gw := NewLLMGateway(testStore())
	gw.UseMiddleware(recorder{"outer", &order}, recorder{"inner", &order})

	handler := RequestHandler(func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		order = append(order, "provider")
		return &llm.Response{}, nil
	})
	for i := len(gw.middlewares) - 1; i >= 0; i-- {
		handler = gw.middlewares[i].HandleRequest(handler)
	}
	_, err := handler(context.Background(), llm.ProviderNameOpenAI, "k", &llm.Request{})

	require.NoError(t, err)
	assert.Equal(t, []string{"outer", "inner", "provider"}, order)
}

type recorder struct {
	name  string
	order *[]string
}

func (r recorder) HandleRequest(next RequestHandler) RequestHandler {
	return func(ctx context.Context, p llm.ProviderName, key string, req *llm.Request) (*llm.Response, error) {
		*r.order = append(*r.order, r.name)
		return next(ctx, p, key, req)
	}
}

func (r recorder) HandleStreamingRequest(next StreamingRequestHandler) StreamingRequestHandler {
	return next
}
