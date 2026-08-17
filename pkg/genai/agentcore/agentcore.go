// Package agentcore exports OpenTelemetry spans to Amazon Bedrock AgentCore /
// CloudWatch GenAI Observability without an ADOT collector. It SigV4-signs OTLP
// protobuf and POSTs it to the CloudWatch X-Ray OTLP endpoint (traces) and,
// when a log group is configured, additionally derives correlated gen_ai
// message log records and POSTs them to the CloudWatch Logs OTLP endpoint —
// the records AgentCore reads to populate a span's Input/Output (the span
// attributes alone are not enough).
//
// It lives in the SDK so standalone SDK users can export to AgentCore too, not
// just the gateway.
//
// The exporter reads the message content from the span's gen_ai.input.messages
// / gen_ai.output.messages attributes, which the SDK emits in the OTel semconv
// role/parts shape (see genai.InputMessages / genai.OutputMessages):
//
//	exp, _ := agentcore.NewExporter(agentcore.Config{Region: "us-east-1", ...})
//	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
//
// Prerequisite (one-time, per AWS account): enable CloudWatch Transaction
// Search and run `aws xray update-trace-segment-destination --destination
// CloudWatchLogs`, otherwise the endpoint accepts spans but nothing surfaces.
package agentcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/hastekit/agent-sdk-go/pkg/genai"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// SigV4 signing service names for the two CloudWatch OTLP endpoints.
	awsXRayService = "xray"
	awsLogsService = "logs"

	// CloudWatch GenAI Observability renders a span's Input/Output from log
	// records correlated by trace/span id, in this stream, under the metric
	// namespace AgentCore uses.
	awsLogStream       = "otel-rt-logs"
	awsMetricNamespace = "bedrock-agentcore"
	genAILogScope      = "hastekit.genai"
	genAISystemAttr    = "gen_ai.system"

	// eventInferenceDetails is the composite gen_ai event whose body carries
	// {input:{messages}, output:{messages}} — the record AgentCore renders a
	// span's Input/Output from.
	eventInferenceDetails = "gen_ai.client.inference.operation.details"
)

// Config is the per-destination AWS configuration for the exporter. Static
// credentials are used directly (no default credential-chain lookup).
type Config struct {
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// LogGroup enables the Input/Output log-events path. Empty = traces only.
	LogGroup string
}

// NewExporter builds an AgentCore span exporter for the given config.
func NewExporter(cfg Config) (sdktrace.SpanExporter, error) {
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		return nil, fmt.Errorf("agentcore: region is required")
	}

	client := &client{
		tracesEndpoint: fmt.Sprintf("https://xray.%s.amazonaws.com/v1/traces", region),
		logsEndpoint:   fmt.Sprintf("https://logs.%s.amazonaws.com/v1/logs", region),
		region:         region,
		logGroup:       strings.TrimSpace(cfg.LogGroup),
		creds: aws.Credentials{
			AccessKeyID:     cfg.AccessKeyID,
			SecretAccessKey: cfg.SecretAccessKey,
			SessionToken:    cfg.SessionToken,
		},
		signer: v4.NewSigner(),
		http:   &http.Client{Timeout: 30 * time.Second},
	}
	return otlptrace.New(context.Background(), client)
}

// client implements otlptrace.Client. The otlptrace exporter hands us
// already-marshaled ResourceSpans (with message attributes already reshaped to
// semconv by the transform), so we own the transport: post the spans to X-Ray,
// and derive+post correlated gen_ai message log records.
type client struct {
	tracesEndpoint string
	logsEndpoint   string
	region         string
	logGroup       string // empty disables the log-events (Input/Output) path
	creds          aws.Credentials
	signer         *v4.Signer
	http           *http.Client

	// streamReady latches true once the log stream is known to exist. The
	// CloudWatch Logs OTLP endpoint does not auto-create streams, so we create
	// it on first use (idempotent) and only skip the create once it succeeds.
	streamReady atomic.Bool
}

func (c *client) Start(ctx context.Context) error { return nil }
func (c *client) Stop(ctx context.Context) error  { return nil }

func (c *client) UploadTraces(ctx context.Context, protoSpans []*tracepb.ResourceSpans) error {
	if len(protoSpans) == 0 {
		return nil
	}

	body, err := proto.Marshal(&coltracepb.ExportTraceServiceRequest{ResourceSpans: protoSpans})
	if err != nil {
		return fmt.Errorf("agentcore: marshal traces: %w", err)
	}
	if err := c.signAndPost(ctx, c.tracesEndpoint, awsXRayService, body, nil); err != nil {
		return err
	}

	// Message content lives in correlated log records, not span attributes.
	// A failure here must not fail the trace export, so it's logged, not
	// returned. Requires a configured log group.
	if c.logGroup != "" {
		if err := c.uploadLogs(ctx, protoSpans); err != nil {
			slog.Warn("agentcore: message log-events export failed",
				slog.String("log_group", c.logGroup), slog.Any("error", err))
		}
	}
	return nil
}

func (c *client) uploadLogs(ctx context.Context, protoSpans []*tracepb.ResourceSpans) error {
	resLogs := buildResourceLogs(protoSpans)
	if len(resLogs) == 0 {
		return nil
	}

	// The OTLP logs endpoint (PutLogEvents underneath) rejects with "log
	// stream does not exist" unless the stream is pre-created. Best-effort;
	// if it fails the post below surfaces the real error.
	c.ensureLogStream(ctx)

	body, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: resLogs})
	if err != nil {
		return fmt.Errorf("agentcore: marshal logs: %w", err)
	}
	headers := map[string]string{
		"x-aws-log-group":        c.logGroup,
		"x-aws-log-stream":       awsLogStream,
		"x-aws-metric-namespace": awsMetricNamespace,
	}
	return c.signAndPost(ctx, c.logsEndpoint, awsLogsService, body, headers)
}

// ensureLogStream creates the log stream via the CloudWatch Logs JSON API
// (CreateLogStream) so the OTLP logs endpoint accepts records. It's idempotent
// — an already-existing stream is treated as success — and latches so the
// create is skipped on subsequent batches once it works. A failure is left
// un-latched so the next batch retries.
func (c *client) ensureLogStream(ctx context.Context) {
	if c.streamReady.Load() {
		return
	}

	payload := fmt.Sprintf(`{"logGroupName":%q,"logStreamName":%q}`, c.logGroup, awsLogStream)
	endpoint := fmt.Sprintf("https://logs.%s.amazonaws.com/", c.region)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "Logs_20140328.CreateLogStream")

	sum := sha256.Sum256([]byte(payload))
	if err := c.signer.SignHTTP(ctx, c.creds, req, hex.EncodeToString(sum[:]), awsLogsService, c.region, time.Now()); err != nil {
		return
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		c.streamReady.Store(true)
		return
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if strings.Contains(string(snippet), "ResourceAlreadyExistsException") {
		c.streamReady.Store(true)
		return
	}
	// Leave un-latched (e.g. the log group itself is missing) → retried next
	// batch; the subsequent logs POST surfaces the underlying error.
	slog.Warn("agentcore: could not ensure log stream",
		slog.String("log_group", c.logGroup), slog.String("status", resp.Status),
		slog.String("detail", strings.TrimSpace(string(snippet))))
}

// signAndPost SigV4-signs a protobuf body for the given service and POSTs it.
func (c *client) signAndPost(ctx context.Context, endpoint, service string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("agentcore: build %s request: %w", service, err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	sum := sha256.Sum256(body)
	if err := c.signer.SignHTTP(ctx, c.creds, req, hex.EncodeToString(sum[:]), service, c.region, time.Now()); err != nil {
		return fmt.Errorf("agentcore: sign %s request: %w", service, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentcore: post %s: %w", service, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("agentcore: %s export rejected: %s: %s", service, resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// buildResourceLogs derives the correlated gen_ai message log records from a
// batch of trace ResourceSpans, reusing each span's resource.
func buildResourceLogs(protoSpans []*tracepb.ResourceSpans) []*logspb.ResourceLogs {
	var out []*logspb.ResourceLogs
	for _, rs := range protoSpans {
		var records []*logspb.LogRecord
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				records = append(records, spanLogRecords(span)...)
			}
		}
		if len(records) == 0 {
			continue
		}
		out = append(out, &logspb.ResourceLogs{
			Resource: rs.GetResource(),
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope:      &commonpb.InstrumentationScope{Name: genAILogScope},
				LogRecords: records,
			}},
		})
	}
	return out
}

// spanLogRecords turns one span's message attributes into a single correlated
// gen_ai inference-details log record. Only chat and invoke_agent spans carry
// renderable messages; everything else is skipped.
func spanLogRecords(span *tracepb.Span) []*logspb.LogRecord {
	var input, output, provider, sessionID, operation string
	for _, kv := range span.GetAttributes() {
		switch kv.GetKey() {
		case genai.AttrInputMessages:
			input = kv.GetValue().GetStringValue()
		case genai.AttrOutputMessages:
			output = kv.GetValue().GetStringValue()
		case genai.AttrProviderName:
			provider = kv.GetValue().GetStringValue()
		case genai.AttrSessionID:
			sessionID = kv.GetValue().GetStringValue()
		case genai.AttrOperationName:
			operation = kv.GetValue().GetStringValue()
		}
	}

	if operation != genai.OpChat && operation != genai.OpInvokeAgent {
		return nil
	}
	body, ok := InferenceDetails(input, output)
	if !ok {
		return nil
	}

	attrs := make([]*commonpb.KeyValue, 0, 2)
	if provider != "" {
		attrs = append(attrs, &commonpb.KeyValue{Key: genAISystemAttr, Value: strValue(provider)})
	}
	if sessionID != "" {
		attrs = append(attrs, &commonpb.KeyValue{Key: genai.AttrSessionID, Value: strValue(sessionID)})
	}

	ts := span.GetEndTimeUnixNano()
	return []*logspb.LogRecord{{
		TimeUnixNano:         ts,
		ObservedTimeUnixNano: ts,
		EventName:            eventInferenceDetails,
		TraceId:              span.GetTraceId(),
		SpanId:               span.GetSpanId(),
		Flags:                span.GetFlags(),
		Body:                 anyValue(body),
		Attributes:           attrs,
	}}
}

// anyValue converts a decoded-JSON Go value into an OTLP AnyValue.
func anyValue(v any) *commonpb.AnyValue {
	switch t := v.(type) {
	case nil:
		return &commonpb.AnyValue{}
	case string:
		return strValue(t)
	case bool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: t}}
	case int:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(t)}}
	case int64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: t}}
	case float64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: t}}
	case map[string]any:
		return kvlistValue(t)
	case []map[string]any:
		arr := make([]*commonpb.AnyValue, 0, len(t))
		for _, e := range t {
			arr = append(arr, kvlistValue(e))
		}
		return arrayValue(arr)
	case []any:
		arr := make([]*commonpb.AnyValue, 0, len(t))
		for _, e := range t {
			arr = append(arr, anyValue(e))
		}
		return arrayValue(arr)
	default:
		return strValue(fmt.Sprintf("%v", t))
	}
}

func kvlistValue(m map[string]any) *commonpb.AnyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic ordering
	kvs := make([]*commonpb.KeyValue, 0, len(m))
	for _, k := range keys {
		kvs = append(kvs, &commonpb.KeyValue{Key: k, Value: anyValue(m[k])})
	}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: kvs}}}
}

func arrayValue(vals []*commonpb.AnyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}
}

func strValue(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}
