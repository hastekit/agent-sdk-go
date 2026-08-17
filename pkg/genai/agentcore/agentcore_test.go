package agentcore

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/genai"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// kvGet looks up a key in an AnyValue that holds a kvlist.
func kvGet(av *commonpb.AnyValue, key string) *commonpb.AnyValue {
	for _, kv := range av.GetKvlistValue().GetValues() {
		if kv.GetKey() == key {
			return kv.GetValue()
		}
	}
	return nil
}

func chatSpan() *tracepb.Span {
	return &tracepb.Span{
		TraceId:         []byte("0123456789abcdef"),
		SpanId:          []byte("01234567"),
		EndTimeUnixNano: 123,
		Attributes: []*commonpb.KeyValue{
			{Key: genai.AttrOperationName, Value: strValue(genai.OpChat)},
			{Key: genai.AttrSessionID, Value: strValue("sess-1")},
			{Key: genai.AttrProviderName, Value: strValue("bedrock")},
			{Key: genai.AttrInputMessages, Value: strValue(`[{"role":"system","parts":[{"type":"text","content":"be nice"}]},{"role":"user","parts":[{"type":"text","content":"this is a test"}]}]`)},
			{Key: genai.AttrOutputMessages, Value: strValue(`[{"role":"assistant","parts":[{"type":"text","content":"hello"}],"finish_reason":"end_turn"}]`)},
		},
	}
}

func TestSpanLogRecords_Composite(t *testing.T) {
	recs := spanLogRecords(chatSpan())
	if len(recs) != 1 {
		t.Fatalf("want 1 composite record, got %d", len(recs))
	}
	r := recs[0]

	if r.EventName != eventInferenceDetails {
		t.Errorf("eventName = %s, want %s", r.EventName, eventInferenceDetails)
	}
	if string(r.TraceId) != "0123456789abcdef" || string(r.SpanId) != "01234567" {
		t.Error("trace/span id not propagated")
	}

	// attributes: gen_ai.system + session.id
	var gotSystem, gotSession string
	for _, a := range r.Attributes {
		switch a.Key {
		case genAISystemAttr:
			gotSystem = a.Value.GetStringValue()
		case genai.AttrSessionID:
			gotSession = a.Value.GetStringValue()
		}
	}
	if gotSystem != "bedrock" {
		t.Errorf("gen_ai.system = %q", gotSystem)
	}
	if gotSession != "sess-1" {
		t.Errorf("session.id = %q, want sess-1", gotSession)
	}

	// body.input.messages: system=string content, user=nested {content:[{text}]}
	inMsgs := kvGet(kvGet(r.Body, "input"), "messages").GetArrayValue().GetValues()
	if len(inMsgs) != 2 {
		t.Fatalf("want 2 input messages, got %d", len(inMsgs))
	}
	if kvGet(inMsgs[0], "role").GetStringValue() != "system" ||
		kvGet(inMsgs[0], "content").GetStringValue() != "be nice" {
		t.Errorf("system message wrong: %v", inMsgs[0])
	}
	if kvGet(inMsgs[1], "role").GetStringValue() != "user" {
		t.Errorf("user role wrong")
	}
	userContentArr := kvGet(kvGet(inMsgs[1], "content"), "content").GetArrayValue().GetValues()
	if len(userContentArr) != 1 || kvGet(userContentArr[0], "text").GetStringValue() != "this is a test" {
		t.Errorf("user nested content wrong: %v", inMsgs[1])
	}

	// body.output.messages: assistant content={message, finish_reason}
	outMsgs := kvGet(kvGet(r.Body, "output"), "messages").GetArrayValue().GetValues()
	if len(outMsgs) != 1 {
		t.Fatalf("want 1 output message, got %d", len(outMsgs))
	}
	outContent := kvGet(outMsgs[0], "content")
	if kvGet(outContent, "message").GetStringValue() != "hello" ||
		kvGet(outContent, "finish_reason").GetStringValue() != "end_turn" {
		t.Errorf("output content wrong: %v", outMsgs[0])
	}
}

func TestSpanLogRecords_SkipsNonAgentSpans(t *testing.T) {
	// A span with messages but an unrelated operation name is skipped.
	span := &tracepb.Span{
		Attributes: []*commonpb.KeyValue{
			{Key: genai.AttrOperationName, Value: strValue("execute_tool")},
			{Key: genai.AttrInputMessages, Value: strValue(`[{"role":"user","parts":[{"type":"text","content":"hi"}]}]`)},
		},
	}
	if recs := spanLogRecords(span); recs != nil {
		t.Errorf("non chat/invoke_agent span should yield no records, got %d", len(recs))
	}
}

func TestBuildResourceLogs(t *testing.T) {
	rs := []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{chatSpan()}}},
	}}
	out := buildResourceLogs(rs)
	if len(out) != 1 || len(out[0].ScopeLogs) != 1 || len(out[0].ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("unexpected resource-logs shape: %#v", out)
	}
	if out[0].ScopeLogs[0].LogRecords[0].EventName != eventInferenceDetails {
		t.Errorf("event name = %s", out[0].ScopeLogs[0].LogRecords[0].EventName)
	}
}
