package openaicompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

func TestClientNewResponses(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody ChatRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = sonic.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"hi there"}}],
			"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	defer server.Close()

	client := NewClient(&ClientOptions{BaseURL: server.URL, ApiKey: "sk-test"})

	out, err := client.NewResponses(context.Background(), &responses.Request{
		Model: "m",
		Input: responses.InputUnion{OfString: utils.Ptr("hi")},
	})
	if err != nil {
		t.Fatalf("NewResponses: %v", err)
	}

	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody.Stream == nil || *gotBody.Stream {
		t.Errorf("stream = %v, want false on a non-streaming call", gotBody.Stream)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Content.Text() != "hi" {
		t.Errorf("messages = %+v", gotBody.Messages)
	}

	if len(out.Output) != 1 || (*out.Output[0].OfOutputMessage.Content)[0].OfOutputText.Text != "hi there" {
		t.Errorf("output = %+v", out.Output)
	}
	if out.Usage.InputTokens != 5 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

func TestClientNewResponsesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","code":"unauthorized"}}`))
	}))
	defer server.Close()

	client := NewClient(&ClientOptions{BaseURL: server.URL, ApiKey: "nope"})

	_, err := client.NewResponses(context.Background(), &responses.Request{Model: "m"})
	if err == nil {
		t.Fatal("NewResponses() error = nil, want the provider's message")
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error = %q, want the provider's message", err)
	}
}

func TestClientNewStreamingResponses(t *testing.T) {
	frames := []string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		`data: [DONE]`,
	}

	var gotStreamOptions *StreamOptions
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req ChatRequest
		_ = sonic.Unmarshal(body, &req)
		gotStreamOptions = req.StreamOptions

		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame+"\n\n")
		}
	}))
	defer server.Close()

	client := NewClient(&ClientOptions{BaseURL: server.URL, ApiKey: "sk-test"})

	stream, err := client.NewStreamingResponses(context.Background(), &responses.Request{
		Model:      "m",
		Input:      responses.InputUnion{OfString: utils.Ptr("hi")},
		Parameters: responses.Parameters{Stream: utils.Ptr(true)},
	})
	if err != nil {
		t.Fatalf("NewStreamingResponses: %v", err)
	}

	var text string
	var completed *responses.ChunkResponseData
	for chunk := range stream {
		if chunk.OfOutputTextDelta != nil {
			text += chunk.OfOutputTextDelta.Delta
		}
		if chunk.OfResponseCompleted != nil {
			completed = &chunk.OfResponseCompleted.Response
		}
	}

	if gotStreamOptions == nil || !gotStreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage was not requested")
	}
	if text != "Hello" {
		t.Errorf("streamed text = %q, want Hello", text)
	}
	if completed == nil {
		t.Fatal("stream ended without response.completed")
	}
	if completed.Usage.TotalTokens != 7 {
		t.Errorf("completed usage = %+v, want the trailing frame's counts", completed.Usage)
	}
}
