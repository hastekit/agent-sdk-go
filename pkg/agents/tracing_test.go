package agents_test

import (
	"context"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	agentExporter     *tracetest.InMemoryExporter
	agentExporterOnce sync.Once
)

// recordingSpans installs an in-memory span exporter as the global tracer
// provider. The package `tracer` delegates to whichever provider is set
// first, so set it exactly once and reset the exporter per test.
func recordingSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	agentExporterOnce.Do(func() {
		agentExporter = tracetest.NewInMemoryExporter()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(agentExporter)))
	})
	agentExporter.Reset()
	return agentExporter
}

// The in-process tool executor brackets every tool call with a GenAI
// execute_tool span via ExecuteWithTrace. Running through ExecuteWithoutTrace
// (the entry callers use to opt out of the run-level span) isolates that tool
// span: no invoke_agent span is opened, since that is Execute's job.
func TestExecuteWithTrace_EmitsToolSpan(t *testing.T) {
	exporter := recordingSpans(t)

	llm := &scriptedLLM{script: []*responses.Response{
		toolCallResponse("call-1", "mytool", `{"x":1}`),
		textResponse("done"),
	}}
	tool := newFakeTool("mytool", false, "tool output")
	agent := agents.NewAgent(&agents.AgentOptions{
		Name:         "traced",
		History:      history.NewConversationManager(history.NewInMemoryConversationPersistence()),
		StreamBroker: streambroker.NewMemoryStreamBroker(),
		Tools:        []agents.Tool{tool},
	}).WithLLM(llm)

	out, err := agent.ExecuteWithoutTrace(context.Background(), &agents.AgentInput{Message: userMessage("hi")})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	requireStatus(t, out, agentstate.RunStatusCompleted)

	var exec tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		if s.Name == "invoke_agent traced" {
			t.Fatal("invoke_agent span should not be emitted by the loop")
		}
		if s.Name == "execute_tool mytool" {
			exec = s
		}
	}
	if exec.Name == "" {
		t.Fatal("execute_tool span was not emitted")
	}
	if got := spanAttr(exec, "gen_ai.tool.name"); got != "mytool" {
		t.Fatalf("execute_tool gen_ai.tool.name = %q, want %q", got, "mytool")
	}
	if got := spanAttr(exec, "gen_ai.tool.call.id"); got != "call-1" {
		t.Fatalf("execute_tool gen_ai.tool.call.id = %q, want %q", got, "call-1")
	}
}

func spanAttr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}
