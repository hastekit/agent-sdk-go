package llm

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// APIError is a non-2xx response from a provider. It exists so callers can
// tell a rate limit from a malformed request without matching on message
// text: the status code is the only thing every provider agrees on.
//
// Message is the provider's own error message where it sent one, so
// err.Error() reads the same as it did before this type existed.
type APIError struct {
	// StatusCode is the HTTP status the provider returned.
	StatusCode int

	// Message is the provider's error message, or a description built from
	// the status and body when the provider sent nothing recognizable.
	Message string

	// RetryAfter is how long the provider asked the caller to wait, from
	// the Retry-After (or retry-after-ms) header. Zero means it said
	// nothing — not that retrying immediately is safe.
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return e.Message }

// AsAPIError reports whether err is, or wraps, an *APIError.
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// StatusCodeOf returns the HTTP status err carries, or 0 for an error that
// did not come from a provider response.
func StatusCodeOf(err error) int {
	if apiErr, ok := AsAPIError(err); ok {
		return apiErr.StatusCode
	}
	return 0
}

// RetryAfterFromHeader reads the delay a provider asked for. It understands
// Retry-After in both of the forms RFC 9110 allows — delay-seconds and an
// HTTP-date — and retry-after-ms, which OpenAI and Anthropic send for
// sub-second waits.
//
// A malformed value, or a date already in the past, reads as zero: a header
// that cannot be understood should not become an unbounded sleep.
func RetryAfterFromHeader(h http.Header) time.Duration {
	return retryAfterFromHeader(h, time.Now())
}

func retryAfterFromHeader(h http.Header, now time.Time) time.Duration {
	if h == nil {
		return 0
	}

	// Milliseconds first: a provider that sends both means the finer one.
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}

	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}

	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}

	if at, err := http.ParseTime(v); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}

	return 0
}
