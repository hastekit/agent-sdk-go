package llm

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_edit"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/image_generation"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every variant must round-trip, so a fallback can retarget any modality.
func TestSetRequestedModelCoversEveryVariant(t *testing.T) {
	cases := []struct {
		name string
		req  *Request
	}{
		{"responses", &Request{OfResponsesInput: &responses.Request{Model: "before"}}},
		{"chat completion", &Request{OfChatCompletionInput: &chat_completion.Request{Model: "before"}}},
		{"embeddings", &Request{OfEmbeddingsInput: &embeddings.Request{Model: "before"}}},
		{"speech", &Request{OfSpeech: &speech.Request{Model: "before"}}},
		{"transcription", &Request{OfTranscription: &transcription.Request{Model: "before"}}},
		{"image generation", &Request{OfImageGeneration: &image_generation.Request{Model: "before"}}},
		{"image edit", &Request{OfImageEdit: &image_edit.Request{Model: "before"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, "before", tc.req.GetRequestedModel())

			tc.req.SetRequestedModel("after")

			assert.Equal(t, "after", tc.req.GetRequestedModel())
		})
	}
}

func TestSetRequestedModelIgnoresAnEmptyModel(t *testing.T) {
	req := &Request{OfResponsesInput: &responses.Request{Model: "gpt-4o-mini"}}

	req.SetRequestedModel("")

	assert.Equal(t, "gpt-4o-mini", req.GetRequestedModel(),
		"a target that names no model keeps the one already asked for")
}

func TestSetRequestedModelOnNil(t *testing.T) {
	var req *Request
	assert.NotPanics(t, func() { req.SetRequestedModel("gpt-4o") })
}

func TestSetRequestedModelOnAnEmptyRequest(t *testing.T) {
	req := &Request{}
	assert.NotPanics(t, func() { req.SetRequestedModel("gpt-4o") })
	assert.Empty(t, req.GetRequestedModel())
}

// --- Clone ----------------------------------------------------------------

func TestCloneIsolatesTheModel(t *testing.T) {
	original := &Request{OfResponsesInput: &responses.Request{Model: "gpt-4o-mini"}}

	clone := original.Clone()
	clone.SetRequestedModel("claude-sonnet-4-5")

	assert.Equal(t, "gpt-4o-mini", original.GetRequestedModel(),
		"the caller still holds the original; retargeting a copy must not reach it")
	assert.Equal(t, "claude-sonnet-4-5", clone.GetRequestedModel())
}

func TestCloneIsolatesEveryVariant(t *testing.T) {
	cases := []struct {
		name string
		req  *Request
	}{
		{"responses", &Request{OfResponsesInput: &responses.Request{Model: "before"}}},
		{"chat completion", &Request{OfChatCompletionInput: &chat_completion.Request{Model: "before"}}},
		{"embeddings", &Request{OfEmbeddingsInput: &embeddings.Request{Model: "before"}}},
		{"speech", &Request{OfSpeech: &speech.Request{Model: "before"}}},
		{"transcription", &Request{OfTranscription: &transcription.Request{Model: "before"}}},
		{"image generation", &Request{OfImageGeneration: &image_generation.Request{Model: "before"}}},
		{"image edit", &Request{OfImageEdit: &image_edit.Request{Model: "before"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clone := tc.req.Clone()
			clone.SetRequestedModel("after")

			assert.Equal(t, "before", tc.req.GetRequestedModel())
			assert.Equal(t, "after", clone.GetRequestedModel())
		})
	}
}

// Clone is documented as a copy you can retarget, not a deep copy. Pinning
// that keeps a later reader from assuming the contents are safe to mutate.
func TestCloneSharesTheRequestContents(t *testing.T) {
	original := &Request{OfResponsesInput: &responses.Request{
		Model:      "gpt-4o-mini",
		Parameters: responses.Parameters{Metadata: map[string]string{"tenant": "acme"}},
	}}

	clone := original.Clone()
	clone.OfResponsesInput.Metadata["tenant"] = "globex"

	assert.Equal(t, "globex", original.OfResponsesInput.Metadata["tenant"],
		"Clone shares everything below the model")
}

func TestCloneOnNil(t *testing.T) {
	var req *Request
	assert.Nil(t, req.Clone())
}

func TestCloneOfAnEmptyRequest(t *testing.T) {
	clone := (&Request{}).Clone()

	require.NotNil(t, clone)
	assert.Empty(t, clone.GetRequestedModel())
}
