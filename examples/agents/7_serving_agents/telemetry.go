package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

func newExporter(w io.Writer) (trace.SpanExporter, error) {
	return stdouttrace.New(
		stdouttrace.WithWriter(w),
		stdouttrace.WithPrettyPrint(),
		stdouttrace.WithoutTimestamps(),
	)
}

func newHttpExporter(endpoint string) (trace.SpanExporter, error) {
	endpointWithProto := strings.Replace(endpoint, "http://", "", 1)
	authKey := fmt.Sprintf("%s:%s", os.Getenv("LANGFUSE_PUBLIC_KEY"), os.Getenv("LANGFUSE_SECRET_KEY"))
	return otlptracehttp.New(
		context.Background(),
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte(authKey))),
		}),
		otlptracehttp.WithEndpoint(endpointWithProto),
		otlptracehttp.WithURLPath("/api/public/otel/v1/traces"),
	)
}

// newOTELCollectorExporter creates an exporter that sends traces to an OTEL collector
func newOTELCollectorExporter(endpoint string) (trace.SpanExporter, error) {
	// Remove protocol prefix if present
	endpointWithProto := strings.Replace(endpoint, "http://", "", 1)
	endpointWithProto = strings.Replace(endpointWithProto, "https://", "", 1)

	return otlptracehttp.New(
		context.Background(),
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithEndpoint(endpointWithProto),
	)
}

func newResource() *resource.Resource {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "agent-server"
	}

	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion("0.1.0"),
	)
}

// NewProvider creates new telemetry provider, and sets it as a default open telemetry trace provider.
//
// Endpoint priority:
// 1. OTEL_EXPORTER_OTLP_ENDPOINT - for OTEL collector (e.g., "localhost:4318")
// 2. LANGFUSE_ENDPOINT - for Langfuse
// 3. Falls back to file-based tracing
//
// Returns a teardown func
func NewProvider(endpoint string) func() {
	f, err := os.Create("traces.txt")
	if err != nil {
		slog.Error("Unable to create traces.txt", slog.Any("error", err))
		return func() {}
	}

	var exp trace.SpanExporter

	// Check for OTEL collector endpoint first
	if endpoint != "" {
		exp, err = newHttpExporter(endpoint)
	} else {
		// Fall back to file-based tracing
		slog.Info("Using file-based tracing (traces.txt)")
		exp, err = newExporter(f)
	}

	if err != nil {
		slog.Error("Unable to create exporter", slog.Any("error", err))
		panic(err)
	}

	// Wrap the deployment-wide exporter in a routing exporter that fans
	// spans out per organisation (default pipeline and/or the org's own
	// external OTLP collector). Until SetOrgConfigResolver is called the
	// router is a pass-through to the default exporter. The enrich
	// processor stamps org_id and session.id from baggage onto every span
	// so whole traces — not just root spans — are routable and
	// session-groupable.
	routing := newRoutingExporter(exp)
	tp := trace.NewTracerProvider(
		trace.WithSampler(trace.ParentBased(trace.AlwaysSample())),
		trace.WithSpanProcessor(trace.NewBatchSpanProcessor(routing)),
		trace.WithResource(newResource()),
	)

	// Surface exporter/processor errors (e.g. a rejected external OTLP
	// export) through slog instead of OTel's default stderr logger, which
	// is easy to miss in structured logs.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("otel error", slog.Any("error", err))
	}))

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),
	)

	return func() {
		if err := f.Close(); err != nil {
			slog.Error("Unable to close traces file", slog.Any("error", err))
		}

		if err := tp.Shutdown(context.Background()); err != nil {
			slog.Error("unable to shutdown trace provider", slog.Any("error", err))
		}
	}
}
