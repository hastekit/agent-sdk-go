package responses_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/require"
)

func TestStreamErrorSurvivesBrokerSerialization(t *testing.T) {
	original := responses.NewStreamError(errors.New("connection reset"))
	data, err := json.Marshal(original)
	require.NoError(t, err)
	var decoded responses.ResponseChunk
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, "error", decoded.ChunkType())
	require.Equal(t, original.OfError, decoded.OfError)
}
