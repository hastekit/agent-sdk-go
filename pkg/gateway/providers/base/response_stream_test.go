package base_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/anthropic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/openai"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/xai"
	"github.com/stretchr/testify/require"
)

type responseProvider interface {
	NewResponses(context.Context, *responses.Request) (*responses.Response, error)
	NewStreamingResponses(context.Context, *responses.Request) (chan *responses.ResponseChunk, error)
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var providers = map[string]func(*http.Client) responseProvider{
	"openai": func(c *http.Client) responseProvider { return openai.NewClient(&openai.ClientOptions{HTTPClient: c}) },
	"anthropic": func(c *http.Client) responseProvider {
		return anthropic.NewClient(&anthropic.ClientOptions{HTTPClient: c})
	},
	"gemini": func(c *http.Client) responseProvider { return gemini.NewClient(&gemini.ClientOptions{HTTPClient: c}) },
	"xai":    func(c *http.Client) responseProvider { return xai.NewClient(&xai.ClientOptions{HTTPClient: c}) },
}

func TestProviderRequestsCarryCancellation(t *testing.T) {
	for name, newProvider := range providers {
		for _, streaming := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/response", true: "/stream"}[streaming], func(t *testing.T) {
				entered := make(chan struct{})
				observed := make(chan error, 1)
				client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
					close(entered)
					select {
					case <-r.Context().Done():
						observed <- r.Context().Err()
						return nil, r.Context().Err()
					case <-time.After(time.Second):
						observed <- nil
						return nil, errors.New("request did not cancel")
					}
				})}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					p := newProvider(client)
					var err error
					if streaming {
						_, err = p.NewStreamingResponses(ctx, &responses.Request{})
					} else {
						_, err = p.NewResponses(ctx, &responses.Request{})
					}
					done <- err
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("request did not reach transport")
				}
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
				require.ErrorIs(t, <-observed, context.Canceled)
			})
		}
	}
}

type trackedBody struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func (b *trackedBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestProviderCancellationReleasesBlockedStreamSend(t *testing.T) {
	payloads := map[string]string{
		"openai":    "data: {\"type\":\"response.created\",\"response\":{}}\n\n",
		"anthropic": "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"content\":[],\"usage\":{}}}\n\n",
		"gemini":    `[{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}]}]`,
		"xai":       "data: {\"type\":\"response.created\",\"response\":{}}\n\n",
	}
	for name, newProvider := range providers {
		t.Run(name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(payloads[name]), closed: make(chan struct{})}
			client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
			})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			chunks, err := newProvider(client).NewStreamingResponses(ctx, &responses.Request{})
			require.NoError(t, err)
			// Receive the first event, then leave the producer with no consumer.
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
		})
	}
}

func TestProviderStreamingIntegrity(t *testing.T) {
	cases := []struct {
		name, provider, payload string
		success                 bool
		message                 string
	}{
		{"openai-complete", "openai", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", true, ""},
		{"openai-empty", "openai", "", false, "unexpected EOF"},
		{"openai-truncated", "openai", "data: {\"type\":\"response.created\",\"response\":{}}\n\n", false, "unexpected EOF"},
		{"openai-malformed", "openai", "data: {broken}\n\n", false, "unexpected EOF"},
		{"openai-failed", "openai", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"provider failed\"}}}\n\n", false, "provider failed"},
		{"openai-incomplete", "openai", "data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", false, "max_output_tokens"},
		{"anthropic-empty", "anthropic", "", false, "unexpected EOF"},
		{"anthropic-error", "anthropic", "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n", false, "overloaded"},
		{"gemini-complete", "gemini", `[{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}]}]`, true, ""},
		{"gemini-truncated", "gemini", `[{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}]}`, false, "EOF"},
		{"gemini-unfinished", "gemini", `[{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}]}]`, false, "unexpected EOF"},
		{"gemini-error", "gemini", `[{"error":{"code":500,"message":"provider failed"}}]`, false, "provider failed"},
		{"xai-complete", "xai", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", true, ""},
		{"xai-truncated", "xai", "data: {\"type\":\"response.created\",\"response\":{}}\n\n", false, "unexpected EOF"},
		{"xai-error", "xai", "data: {\"type\":\"error\",\"message\":\"provider failed\"}\n\n", false, "provider failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.payload))}, nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			chunks, err := providers[tc.provider](client).NewStreamingResponses(ctx, &responses.Request{})
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
			require.Equal(t, tc.success, completed)
			if tc.success {
				require.Nil(t, failure)
			} else {
				require.NotNil(t, failure)
				require.Contains(t, failure.Message, tc.message)
			}
		})
	}
}

func TestProviderCancellationClosesIdleResponseBody(t *testing.T) {
	for name, newProvider := range providers {
		t.Run(name, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, nil
			})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			chunks, err := newProvider(client).NewStreamingResponses(ctx, &responses.Request{})
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
		})
	}
}

func TestProviderStreamingToleratesUnknownEvents(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		for _, terminal := range []string{"complete", "failed", "truncated"} {
			t.Run(provider+"/"+terminal, func(t *testing.T) {
				// Include unknown field shapes as well as unknown types: error parsing
				// must not interpret these payloads as provider failures.
				payload := "data: {\"type\":\"future.event\",\"response\":\"new shape\",\"error\":42}\n\n"
				switch terminal {
				case "complete":
					if provider == "openai" {
						payload += "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
					} else {
						payload += "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"content\":[],\"usage\":{}}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
					}
				case "failed":
					payload += "data: {\"type\":\"error\",\"message\":\"provider failed\"}\n\n"
				}
				client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
				})}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				stream, err := providers[provider](client).NewStreamingResponses(ctx, &responses.Request{})
				require.NoError(t, err)
				completed := false
				var failure *responses.StreamError
				for chunk := range stream {
					if chunk.OfResponseCompleted != nil {
						completed = true
					}
					if chunk.OfError != nil {
						failure = chunk.OfError
					}
				}
				require.Equal(t, terminal == "complete", completed)
				switch terminal {
				case "complete":
					require.Nil(t, failure)
				case "failed":
					require.NotNil(t, failure)
					require.Contains(t, failure.Message, "provider failed")
				case "truncated":
					require.NotNil(t, failure)
					require.Contains(t, failure.Message, "unexpected EOF")
				}
			})
		}
	}
}

// TestProviderStreamingSurvivesDamagedFrames pins the tolerance the
// line-oriented parsers used to have. A frame that is not valid JSON says
// nothing about the health of the stream, so it must not discard the events
// around it — including the completion event that decides whether the run
// succeeded.
func TestProviderStreamingSurvivesDamagedFrames(t *testing.T) {
	completion := map[string]string{
		"openai":    "data: {\"type\":\"response.completed\",\"response\":{}}\n\n",
		"anthropic": "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"content\":[],\"usage\":{}}}\n\ndata: {\"type\":\"message_stop\"}\n\n",
		"xai":       "data: {\"type\":\"response.completed\",\"response\":{}}\n\n",
	}
	damage := map[string]string{
		"empty-data-frame":    "data:\n\n",
		"non-json-keepalive":  "data: ping\n\n",
		"truncated-json":      "data: {\"type\":\"response.outp\n\n",
		"corrupt-json":        "data: {broken}\n\n",
		"bare-array":          "data: [1,2,3]\n\n",
		"comment-and-garbage": ": keep-alive\ndata: <html>502</html>\n\n",
	}
	for provider := range completion {
		for name, frame := range damage {
			t.Run(provider+"/"+name, func(t *testing.T) {
				client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
					body := frame + completion[provider]
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				stream, err := providers[provider](client).NewStreamingResponses(ctx, &responses.Request{})
				require.NoError(t, err)
				completed := false
				for chunk := range stream {
					if chunk.OfResponseCompleted != nil {
						completed = true
					}
					require.Nil(t, chunk.OfError, "a damaged frame must not fail the stream")
				}
				require.True(t, completed, "completion event lost after a damaged frame")
			})
		}
	}
}

// TestProviderStreamingAcceptsEventsWithoutBlankLines covers providers and
// proxies that write one event per line and omit the blank separator. The SSE
// spec says to join a frame's data lines, but joining independent payloads
// yields nonsense, so the parser falls back to one event per line.
func TestProviderStreamingAcceptsEventsWithoutBlankLines(t *testing.T) {
	payloads := map[string]string{
		"openai": "data: {\"type\":\"response.created\",\"response\":{}}\n" +
			"data: {\"type\":\"response.completed\",\"response\":{}}\n",
		"anthropic": "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"content\":[],\"usage\":{}}}\n" +
			"data: {\"type\":\"message_stop\"}\n",
		"xai": "data: {\"type\":\"response.created\",\"response\":{}}\n" +
			"data: {\"type\":\"response.completed\",\"response\":{}}\n",
	}
	for provider, payload := range payloads {
		t.Run(provider, func(t *testing.T) {
			client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			stream, err := providers[provider](client).NewStreamingResponses(ctx, &responses.Request{})
			require.NoError(t, err)
			completed := false
			for chunk := range stream {
				if chunk.OfResponseCompleted != nil {
					completed = true
				}
				require.Nil(t, chunk.OfError)
			}
			require.True(t, completed)
		})
	}
}

// TestResponseStreamJoinsMultiLineDataFrames keeps the spec-conformant path
// working: a single event whose payload is split across data lines is joined.
func TestResponseStreamJoinsMultiLineDataFrames(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\ndata: \"response\":{}}\n\n"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream := base.StreamResponsesSSE(ctx, body, func(data []byte) ([]*responses.ResponseChunk, error) {
		var chunk responses.ResponseChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return nil, err
		}
		return []*responses.ResponseChunk{&chunk}, nil
	})
	completed := false
	for chunk := range stream {
		if chunk.OfResponseCompleted != nil {
			completed = true
		}
		require.Nil(t, chunk.OfError)
	}
	require.True(t, completed, "multi-line data frame was not joined")
}

// TestResponseStreamReportsDoneWithoutCompletion keeps the truncation signal
// specific: [DONE] arriving with no completion event is a truncated response,
// and the message should say so rather than blaming the transport.
func TestResponseStreamReportsDoneWithoutCompletion(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: [DONE]\n\n"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream := base.StreamResponsesSSE(ctx, body, func(data []byte) ([]*responses.ResponseChunk, error) {
		var chunk responses.ResponseChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return nil, err
		}
		return []*responses.ResponseChunk{&chunk}, nil
	})
	var failure *responses.StreamError
	for chunk := range stream {
		if chunk.OfError != nil {
			failure = chunk.OfError
		}
	}
	require.NotNil(t, failure)
	require.Contains(t, failure.Message, "[DONE] without a completion event")
}
