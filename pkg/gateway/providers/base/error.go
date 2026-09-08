package base

import (
	"fmt"
	"io"
	"net/http"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
)

// ParseErrorResponse turns a non-2xx provider HTTP response into a
// descriptive error. It reads (and so allows the caller to close) the
// response body and never panics on an unexpected body shape: when the
// provider's usual {"error":{"message":...}} envelope is absent — e.g. a
// proxy returns HTML, a gateway returns plain text, or the body is empty —
// it falls back to the HTTP status code and the raw body.
//
// Both the object form ({"error":{"message":...}}) and the array form
// ([{"error":{"message":...}}], used by some Google/Gemini endpoints) are
// recognized.
//
// The result is always an *llm.APIError, so a caller can tell a rate limit
// from a bad request without reading the message. The message itself is
// unchanged from what this function has always produced.
func ParseErrorResponse(res *http.Response) error {
	body, _ := io.ReadAll(res.Body)

	return &llm.APIError{
		StatusCode: res.StatusCode,
		Message:    errorMessage(res.StatusCode, body),
		RetryAfter: llm.RetryAfterFromHeader(res.Header),
	}
}

// errorMessage builds the human-readable half, preferring the provider's own
// message and falling back to the status and raw body.
func errorMessage(statusCode int, body []byte) string {
	if msg := extractErrorMessage(body); msg != "" {
		return msg
	}
	if len(body) > 0 {
		return fmt.Sprintf("request failed with status %d: %s", statusCode, string(body))
	}
	return fmt.Sprintf("request failed with status %d", statusCode)
}

func extractErrorMessage(body []byte) string {
	var asObject map[string]any
	if err := sonic.Unmarshal(body, &asObject); err == nil {
		if msg := messageFromEnvelope(asObject); msg != "" {
			return msg
		}
	}

	var asArray []map[string]any
	if err := sonic.Unmarshal(body, &asArray); err == nil && len(asArray) > 0 {
		if msg := messageFromEnvelope(asArray[0]); msg != "" {
			return msg
		}
	}

	return ""
}

// messageFromEnvelope reads the error envelopes providers actually send:
//
//	{"error": {"message": "..."}}   OpenAI, Anthropic, Gemini, xAI
//	{"error": "..."}                the same, sent as a bare string
//	{"detail": {"message": "..."}}  ElevenLabs, and FastAPI services generally
//	{"detail": "..."}               the same, sent as a bare string
//	{"message": "..."}              AWS Bedrock
//
// Only a non-2xx body reaches here, so reading a top-level "message" cannot
// pick up a field from a successful response.
func messageFromEnvelope(obj map[string]any) string {
	for _, key := range []string{"error", "detail"} {
		switch v := obj[key].(type) {
		case map[string]any:
			if msg, ok := v["message"].(string); ok && msg != "" {
				return msg
			}
		case string:
			if v != "" {
				return v
			}
		}
	}

	if msg, ok := obj["message"].(string); ok {
		return msg
	}
	return ""
}
