package responses

import (
	"encoding/json"
	"fmt"
)

// StreamError is a terminal stream failure, including failures after HTTP 200.
type StreamError struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func (e *StreamError) Error() string { return e.Message }

func NewStreamError(err error) *ResponseChunk {
	return &ResponseChunk{OfError: &StreamError{Type: "error", Code: "stream_error", Message: err.Error()}}
}

// ParseStreamError normalizes OpenAI and Anthropic error events, and OpenAI's
// failed/incomplete response events. A non-error event returns nil.
func ParseStreamError(data []byte) (*StreamError, error) {
	// Inspect only the discriminator before decoding error-specific fields.
	// Future non-error events may use those field names with different shapes.
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	switch header.Type {
	case "error", "response.failed", "response.incomplete":
	default:
		return nil, nil
	}

	var event struct {
		Type     string `json:"type"`
		Code     string `json:"code"`
		Message  string `json:"message"`
		Error    *Error `json:"error"`
		Response struct {
			Error             *Error `json:"error"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	switch event.Type {
	case "error", "response.failed", "response.incomplete":
		result := &StreamError{Type: "error", Code: event.Code, Message: event.Message}
		detail := event.Error
		if detail == nil {
			detail = event.Response.Error
		}
		if detail != nil {
			result.Code, result.Message = detail.Code, detail.Message
		}
		if result.Message == "" {
			result.Message = event.Type
		}
		if reason := event.Response.IncompleteDetails.Reason; reason != "" {
			result.Message = fmt.Sprintf("%s: %s", result.Message, reason)
		}
		return result, nil
	default:
		return nil, nil
	}
}
