package bedrock_test

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/bedrock"
	"github.com/stretchr/testify/require"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// encodeEventStreamMessage builds one AWS event stream frame:
// [total_length:4][headers_length:4][prelude_crc:4][headers][payload][message_crc:4]
func encodeEventStreamMessage(headers map[string]string, payload string) []byte {
	var head []byte
	for name, value := range headers {
		head = append(head, byte(len(name)))
		head = append(head, name...)
		head = append(head, 7) // string
		head = binary.BigEndian.AppendUint16(head, uint16(len(value)))
		head = append(head, value...)
	}
	body := append(append([]byte{}, head...), payload...)

	prelude := make([]byte, 0, 12)
	prelude = binary.BigEndian.AppendUint32(prelude, uint32(12+len(body)+4))
	prelude = binary.BigEndian.AppendUint32(prelude, uint32(len(head)))
	prelude = binary.BigEndian.AppendUint32(prelude, crc32.ChecksumIEEE(prelude[0:8]))

	frame := append(append([]byte{}, prelude...), body...)
	return binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
}

func event(eventType, payload string) []byte {
	return encodeEventStreamMessage(map[string]string{":message-type": "event", ":event-type": eventType}, payload)
}

func exception(kind, payload string) []byte {
	return encodeEventStreamMessage(map[string]string{":message-type": "exception", ":exception-type": kind}, payload)
}

// corrupt flips a payload byte, leaving the prelude intact so the frame fails
// its message CRC rather than its length check.
func corrupt(frame []byte) []byte {
	damaged := append([]byte{}, frame...)
	damaged[len(damaged)-5] ^= 0xff
	return damaged
}

func stream(frames ...[]byte) string {
	var b strings.Builder
	for _, f := range frames {
		b.Write(f)
	}
	return b.String()
}

var (
	messageStart      = event("messageStart", `{"role":"assistant"}`)
	contentBlockDelta = event("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hello"}}`)
	contentBlockStop  = event("contentBlockStop", `{"contentBlockIndex":0}`)
	messageStop       = event("messageStop", `{"stopReason":"end_turn"}`)
	metadata          = event("metadata", `{"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}`)
)

func collect(t *testing.T, body string) (bool, *responses.StreamError) {
	t.Helper()
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	chunks, err := bedrock.NewClient(&bedrock.ClientOptions{HTTPClient: client}).
		NewStreamingResponses(ctx, &responses.Request{Model: "anthropic.claude-sonnet-4"})
	require.NoError(t, err)
	completed := false
	var failure *responses.StreamError
	for chunk := range chunks {
		if chunk.OfResponseCompleted != nil {
			completed = true
		}
		if chunk.OfError != nil {
			failure = chunk.OfError
		}
	}
	return completed, failure
}

func TestBedrockStreamingIntegrity(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		success bool
		message string
	}{
		{
			name:    "complete",
			body:    stream(messageStart, contentBlockDelta, contentBlockStop, messageStop, metadata),
			success: true,
		},
		{
			// Without the terminal metadata event the response never completed,
			// which a clean EOF alone cannot distinguish from success.
			name:    "truncated-before-metadata",
			body:    stream(messageStart, contentBlockDelta, contentBlockStop, messageStop),
			message: "unexpected EOF",
		},
		{
			name:    "empty",
			body:    "",
			message: "unexpected EOF",
		},
		{
			name:    "exception",
			body:    stream(messageStart, exception("throttlingException", `{"message":"Too many requests"}`)),
			message: "bedrock throttlingException: Too many requests",
		},
		{
			name:    "exception-unknown-shape",
			body:    stream(exception("internalServerException", `{"detail":"boom"}`)),
			message: `bedrock internalServerException: {"detail":"boom"}`,
		},
		{
			// Bedrock adds event types independently of SDK releases.
			name:    "unknown-event-type",
			body:    stream(messageStart, event("futureEvent", `{"shape":"new"}`), contentBlockDelta, contentBlockStop, messageStop, metadata),
			success: true,
		},
		{
			name:    "undecodable-event-payload",
			body:    stream(messageStart, event("contentBlockDelta", `{broken}`), contentBlockStop, messageStop, metadata),
			success: true,
		},
		{
			name:    "truncated-frame",
			body:    stream(messageStart) + string(contentBlockDelta[:len(contentBlockDelta)/2]),
			message: "unexpected EOF",
		},
		{
			name:    "corrupt-frame",
			body:    stream(messageStart, corrupt(contentBlockDelta)),
			message: "CRC mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			completed, failure := collect(t, tc.body)
			require.Equal(t, tc.success, completed)
			if tc.success {
				require.Nil(t, failure)
				return
			}
			require.NotNil(t, failure)
			require.Contains(t, failure.Message, tc.message)
		})
	}
}

func TestBedrockRequestCarriesCancellation(t *testing.T) {
	entered := make(chan struct{})
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := bedrock.NewClient(&bedrock.ClientOptions{HTTPClient: client}).
			NewStreamingResponses(ctx, &responses.Request{})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach transport")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestBedrockCancellationClosesIdleResponseBody(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks, err := bedrock.NewClient(&bedrock.ClientOptions{HTTPClient: client}).
		NewStreamingResponses(ctx, &responses.Request{})
	require.NoError(t, err)
	cancel()
	done := make(chan struct{})
	go func() {
		for range chunks {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled stream remained blocked reading its body")
	}
}

// TestBedrockCancellationReleasesBlockedStreamSend covers a consumer that walks
// away mid-stream: the producer must not park forever on an unread channel.
func TestBedrockCancellationReleasesBlockedStreamSend(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader(stream(messageStart, contentBlockDelta, contentBlockStop, messageStop, metadata)), closed: make(chan struct{})}
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks, err := bedrock.NewClient(&bedrock.ClientOptions{HTTPClient: client}).
		NewStreamingResponses(ctx, &responses.Request{})
	require.NoError(t, err)
	select {
	case _, ok := <-chunks:
		require.True(t, ok)
	case <-time.After(time.Second):
		t.Fatal("missing first event")
	}
	cancel()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("body leaked after cancellation")
	}
	for range chunks {
	}
}

type trackedBody struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (b *trackedBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }
