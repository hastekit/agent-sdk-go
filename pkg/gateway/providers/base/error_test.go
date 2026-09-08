package base

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func response(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestParseErrorResponseKeepsTheProviderMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// OpenAI, Anthropic, Gemini, xAI.
		{"object envelope", `{"error":{"message":"rate limit reached"}}`, "rate limit reached"},
		// Some Google endpoints answer with an array.
		{"array envelope", `[{"error":{"message":"quota exceeded"}}]`, "quota exceeded"},
		{"error as a string", `{"error":"model not found"}`, "model not found"},
		// ElevenLabs, and FastAPI services generally.
		{"detail envelope", `{"detail":{"message":"voice not found"}}`, "voice not found"},
		{"detail as a string", `{"detail":"not authenticated"}`, "not authenticated"},
		// AWS Bedrock.
		{"top-level message", `{"message":"The security token is invalid"}`, "The security token is invalid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ParseErrorResponse(response(429, tc.body, nil))
			assert.Equal(t, tc.want, err.Error())
		})
	}
}

func TestParseErrorResponseFallsBackToStatusAndBody(t *testing.T) {
	err := ParseErrorResponse(response(502, "<html>bad gateway</html>", nil))
	assert.Equal(t, "request failed with status 502: <html>bad gateway</html>", err.Error())

	err = ParseErrorResponse(response(503, "", nil))
	assert.Equal(t, "request failed with status 503", err.Error())
}

func TestParseErrorResponseCarriesStatusAndRetryAfter(t *testing.T) {
	header := http.Header{"Retry-After": {"20"}}
	err := ParseErrorResponse(response(429, `{"error":{"message":"slow down"}}`, header))

	apiErr, ok := llm.AsAPIError(err)
	require.True(t, ok, "provider errors must be classifiable by status")
	assert.Equal(t, 429, apiErr.StatusCode)
	assert.Equal(t, 20*time.Second, apiErr.RetryAfter)
	assert.Equal(t, "slow down", apiErr.Message)
}

func TestParseErrorResponseWithoutRetryAfter(t *testing.T) {
	err := ParseErrorResponse(response(400, `{"error":{"message":"bad request"}}`, nil))

	apiErr, ok := llm.AsAPIError(err)
	require.True(t, ok)
	assert.Equal(t, 400, apiErr.StatusCode)
	assert.Zero(t, apiErr.RetryAfter)
}

func TestParseErrorResponseIgnoresAnEnvelopeWithoutAMessage(t *testing.T) {
	// An envelope whose message is missing or is not a string must fall back
	// to the status and body rather than reporting an empty error.
	cases := []string{
		`{"error":{"code":429}}`,
		`{"error":{"message":null}}`,
		`{"detail":{}}`,
		`{"error":""}`,
		`{}`,
	}

	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			err := ParseErrorResponse(response(503, body, nil))
			assert.Equal(t, "request failed with status 503: "+body, err.Error())
		})
	}
}

func TestParseErrorResponsePrefersTheNestedMessage(t *testing.T) {
	// A body carrying both shapes is a nested envelope, not a bare one.
	err := ParseErrorResponse(response(400, `{"error":{"message":"nested"},"message":"top"}`, nil))
	assert.Equal(t, "nested", err.Error())
}
