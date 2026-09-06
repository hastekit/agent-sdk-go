package elevenlabs_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	speech2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	transcription2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/elevenlabs"
	"github.com/stretchr/testify/require"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newClient(c *http.Client) *elevenlabs.Client {
	return elevenlabs.NewClient(&elevenlabs.ClientOptions{HTTPClient: c})
}

// TestClientUsesConfiguredHTTPClient guards the option itself: without it a
// caller's timeouts and transport are silently dropped for http.DefaultClient.
func TestClientUsesConfiguredHTTPClient(t *testing.T) {
	used := false
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		used = true
		return nil, errors.New("reached the configured transport")
	})}
	_, err := newClient(client).NewSpeech(context.Background(), &speech2.Request{})
	require.Error(t, err)
	require.True(t, used, "configured HTTPClient was not used")
}

func TestRequestsCarryCancellation(t *testing.T) {
	calls := map[string]func(context.Context, *elevenlabs.Client) error{
		"speech": func(ctx context.Context, c *elevenlabs.Client) error {
			_, err := c.NewSpeech(ctx, &speech2.Request{})
			return err
		},
		"streaming-speech": func(ctx context.Context, c *elevenlabs.Client) error {
			_, err := c.NewStreamingSpeech(ctx, &speech2.Request{})
			return err
		},
		"transcription": func(ctx context.Context, c *elevenlabs.Client) error {
			_, err := c.NewTranscription(ctx, &transcription2.Request{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
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
			go func() { done <- call(ctx, newClient(client)) }()
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

type trackedBody struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func (b *trackedBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

// TestStreamingSpeechCancellationReleasesBlockedSend covers a consumer that
// walks away mid-stream. ElevenLabs streams raw audio in 4KB reads, so a body
// larger than one read leaves the producer mid-loop with nobody receiving.
func TestStreamingSpeechCancellationReleasesBlockedSend(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader(strings.Repeat("a", 32*1024)), closed: make(chan struct{})}
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks, err := newClient(client).NewStreamingSpeech(ctx, &speech2.Request{})
	require.NoError(t, err)
	select {
	case _, ok := <-chunks:
		require.True(t, ok)
	case <-time.After(time.Second):
		t.Fatal("missing first audio chunk")
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

func TestStreamingSpeechDeliversAudio(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("audio-bytes"))}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	chunks, err := newClient(client).NewStreamingSpeech(ctx, &speech2.Request{})
	require.NoError(t, err)
	var got strings.Builder
	for chunk := range chunks {
		require.NotNil(t, chunk.OfAudioDelta)
		got.WriteString(chunk.OfAudioDelta.Audio)
	}
	require.Equal(t, "audio-bytes", got.String())
}
