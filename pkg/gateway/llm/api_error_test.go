package llm

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsAPIErrorFindsAWrappedError(t *testing.T) {
	base := &APIError{StatusCode: 429, Message: "slow down"}
	wrapped := fmt.Errorf("calling provider: %w", base)

	got, ok := AsAPIError(wrapped)
	require.True(t, ok)
	assert.Equal(t, 429, got.StatusCode)
	assert.Equal(t, 429, StatusCodeOf(wrapped))
}

func TestAsAPIErrorIgnoresOtherErrors(t *testing.T) {
	_, ok := AsAPIError(errors.New("plain"))
	assert.False(t, ok)
	assert.Zero(t, StatusCodeOf(errors.New("plain")))
	assert.Zero(t, StatusCodeOf(nil))
}

func TestRetryAfterFromHeader(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"absent", http.Header{}, 0},
		{"nil header", nil, 0},
		{"delay seconds", http.Header{"Retry-After": {"12"}}, 12 * time.Second},
		{"fractional seconds", http.Header{"Retry-After": {"1.5"}}, 1500 * time.Millisecond},
		{"milliseconds win", http.Header{"Retry-After": {"12"}, "Retry-After-Ms": {"250"}}, 250 * time.Millisecond},
		{"http date", http.Header{"Retry-After": {now.Add(30 * time.Second).Format(http.TimeFormat)}}, 30 * time.Second},
		{"past date", http.Header{"Retry-After": {now.Add(-time.Hour).Format(http.TimeFormat)}}, 0},
		{"garbage", http.Header{"Retry-After": {"soon"}}, 0},
		{"negative", http.Header{"Retry-After": {"-5"}}, 0},
		{"zero", http.Header{"Retry-After": {"0"}}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, retryAfterFromHeader(tc.header, now))
		})
	}
}
