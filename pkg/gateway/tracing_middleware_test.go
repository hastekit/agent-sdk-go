package gateway

import (
	"context"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/chat_completion"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	gwExporter     *tracetest.InMemoryExporter
	gwExporterOnce sync.Once
)

// withRecordingTracer installs an in-memory span exporter as the global
// tracer provider. The package `tracer` delegates to whichever provider is
// set first, so we set it exactly once and reset the exporter per test.
func withRecordingTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	gwExporterOnce.Do(func() {
		gwExporter = tracetest.NewInMemoryExporter()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(gwExporter)))
	})
	gwExporter.Reset()
	return gwExporter
}

func spanAttr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

// A non-streaming chat request produces one "chat <model>" span with the
// request and response attributes the middleware derives from the request and
// the returned response.
func TestTracingMiddleware_NonStreaming(t *testing.T) {
	exporter := withRecordingTracer(t)

	next := func(ctx context.Context, _ llm.ProviderName, _ string, _ *llm.Request) (*llm.Response, error) {
		return &llm.Response{OfChatCompletionOutput: &chat_completion.Response{
			ID:      "resp-1",
			Model:   "gpt-4o",
			Choices: []chat_completion.Choice{{FinishReason: "stop"}},
			Usage:   chat_completion.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}}, nil
	}

	handler := NewTracingMiddleware().HandleRequest(next)
	_, err := handler(context.Background(), "openai", "key", &llm.Request{
		OfChatCompletionInput: &chat_completion.Request{
			Model:       "gpt-4o",
			Temperature: utils.Ptr(0.7),
		},
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	s := spans[0]
	if s.Name != "chat gpt-4o" {
		t.Fatalf("span name = %q, want %q", s.Name, "chat gpt-4o")
	}
	checks := map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "openai",
		"gen_ai.request.model":  "gpt-4o",
		"hastekit.request_type": "Chat",
		"gen_ai.response.model": "gpt-4o",
		"gen_ai.response.id":    "resp-1",
	}
	for k, want := range checks {
		if got := spanAttr(s, k); got != want {
			t.Fatalf("span attr %q = %q, want %q", k, got, want)
		}
	}
}

// A provider error is recorded on the span and the request-type reflects a
// non-streaming responses call.
func TestTracingMiddleware_RecordsError(t *testing.T) {
	exporter := withRecordingTracer(t)

	next := func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.Response, error) {
		return nil, context.DeadlineExceeded
	}
	handler := NewTracingMiddleware().HandleRequest(next)
	_, err := handler(context.Background(), "anthropic", "key", &llm.Request{
		OfResponsesInput: &responses.Request{Model: "claude"},
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if got := spanAttr(spans[0], "hastekit.request_type"); got != "Responses" {
		t.Fatalf("request_type = %q, want %q", got, "Responses")
	}
	if len(spans[0].Events) == 0 {
		t.Fatal("expected an error event recorded on the span")
	}
}

// A streaming request wraps the provider channel, forwards every chunk, and
// ends the span once the channel drains.
func TestTracingMiddleware_Streaming(t *testing.T) {
	exporter := withRecordingTracer(t)

	provided := make(chan *responses.ResponseChunk, 2)
	provided <- &responses.ResponseChunk{}
	provided <- &responses.ResponseChunk{}
	close(provided)

	next := func(context.Context, llm.ProviderName, string, *llm.Request) (*llm.StreamingResponse, error) {
		return &llm.StreamingResponse{ResponsesStreamData: provided}, nil
	}
	handler := NewTracingMiddleware().HandleStreamingRequest(next)
	resp, err := handler(context.Background(), "openai", "key", &llm.Request{
		OfResponsesInput: &responses.Request{Model: "gpt-4o"},
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	got := 0
	for range resp.ResponsesStreamData {
		got++
	}
	if got != 2 {
		t.Fatalf("forwarded %d chunks, want 2", got)
	}

	// span.End runs before the wrapped channel closes, so the span is
	// recorded by the time the range above completes.
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Name != "chat gpt-4o" {
		t.Fatalf("span name = %q, want %q", spans[0].Name, "chat gpt-4o")
	}
	if got := spanAttr(spans[0], "hastekit.request_type"); got != "Responses (Stream)" {
		t.Fatalf("request_type = %q, want %q", got, "Responses (Stream)")
	}
}
