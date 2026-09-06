package agents_test

import (
	"context"
	"io"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func TestReadStreamRejectsPrematureEOF(t *testing.T) {
	for _, partial := range []bool{false, true} {
		stream := make(chan *responses.ResponseChunk, 1)
		if partial {
			stream <- textItemDone("msg", "partial answer")
		}
		close(stream)
		acc := agents.Accumulator{}
		out, err := acc.ReadStream(context.Background(), stream, func(*responses.ResponseChunk) {})
		require.Nil(t, out)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	}
}

func TestReadStreamPreservesProviderError(t *testing.T) {
	stream := make(chan *responses.ResponseChunk, 1)
	stream <- &responses.ResponseChunk{OfError: &responses.StreamError{Type: "error", Code: "overloaded", Message: "try later"}}
	close(stream)
	acc := agents.Accumulator{}
	out, err := acc.ReadStream(context.Background(), stream, func(*responses.ResponseChunk) {})
	require.Nil(t, out)
	var streamErr *responses.StreamError
	require.ErrorAs(t, err, &streamErr)
	require.Equal(t, "overloaded", streamErr.Code)
}
