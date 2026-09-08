package base_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/stretchr/testify/require"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func TestResponseStreamReportsReadFailureAfterPartialOutput(t *testing.T) {
	body := io.NopCloser(io.MultiReader(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"), failingReader{}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream := base.StreamResponsesSSE(ctx, body, func(data []byte) ([]*responses.ResponseChunk, error) {
		var chunk responses.ResponseChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return nil, err
		}
		return []*responses.ResponseChunk{&chunk}, nil
	})
	var chunks []*responses.ResponseChunk
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	require.Len(t, chunks, 2)
	require.NotNil(t, chunks[0].OfOutputTextDelta)
	require.NotNil(t, chunks[1].OfError)
	require.Contains(t, chunks[1].OfError.Message, "connection reset by peer")
}
