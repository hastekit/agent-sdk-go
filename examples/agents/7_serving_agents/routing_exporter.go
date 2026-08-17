package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/trace"
)

// orgIDAttrKey is the span attribute used to attribute a span to an
// organisation. Handlers set it explicitly and orgEnrichProcessor
// stamps it onto descendant spans from baggage.
const orgIDAttrKey = "org_id"

// OrgExportTarget is the per-org export decision consumed by the
// routing exporter. It is intentionally decoupled from the otelconfig
// service type to avoid an import cycle (telemetry is a low-level pkg).
type OrgExportTarget struct {
	// ExportToDefault keeps sending the org's spans to the
	// deployment-wide pipeline (internal collector -> ClickHouse).
	ExportToDefault bool
	// ExternalEnabled additionally sends spans to the org's own OTLP
	// collector.
	ExternalEnabled  bool
	ExternalEndpoint string
	ExternalInsecure bool
	ExternalHeaders  map[string]string
}

// OrgConfigResolver maps an org id (as carried on the span) to its
// export target. The bool is false when the id is unknown/unparseable,
// in which case the routing exporter falls back to default-only.
type OrgConfigResolver func(orgID string) (*OrgExportTarget, bool)

// orgResolver holds the injected resolver. It is set after the DB-backed
// service is constructed (NewProvider runs before the DB exists). Until
// then it is nil and routing is default-only — today's behaviour.
var orgResolver atomic.Pointer[OrgConfigResolver]

// SetOrgConfigResolver wires the per-org export resolver into the global
// routing exporter. Safe to call once the services layer is up.
func SetOrgConfigResolver(fn OrgConfigResolver) {
	if fn == nil {
		orgResolver.Store(nil)
		return
	}
	orgResolver.Store(&fn)
}

func resolveOrgTarget(orgID string) (*OrgExportTarget, bool) {
	p := orgResolver.Load()
	if p == nil || orgID == "" {
		return nil, false
	}
	return (*p)(orgID)
}

// routingExporter fans a batch of spans out per organisation: to the
// default pipeline and/or the org's own external OTLP collector,
// according to the resolved config. Spans with no org_id, or when no
// resolver is wired, go to the default exporter only.
type routingExporter struct {
	def trace.SpanExporter

	mu    sync.Mutex
	cache map[string]*cachedExporter // keyed by org id
}

type cachedExporter struct {
	hash string
	exp  trace.SpanExporter
}

func newRoutingExporter(def trace.SpanExporter) *routingExporter {
	return &routingExporter{
		def:   def,
		cache: make(map[string]*cachedExporter),
	}
}

func (e *routingExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}

	// Group spans by org id so each external exporter receives a single
	// batch rather than one call per span.
	byOrg := make(map[string][]trace.ReadOnlySpan)
	for _, s := range spans {
		byOrg[spanOrgID(s)] = append(byOrg[spanOrgID(s)], s)
	}

	var (
		defaultBatch []trace.ReadOnlySpan
		errs         []error
	)

	for orgID, group := range byOrg {
		target, ok := resolveOrgTarget(orgID)
		if !ok {
			// Unknown org or no resolver wired -> default-only.
			defaultBatch = append(defaultBatch, group...)
			continue
		}

		if target.ExportToDefault {
			defaultBatch = append(defaultBatch, group...)
		}

		if target.ExternalEnabled && strings.TrimSpace(target.ExternalEndpoint) != "" {
			exp, err := e.exporterFor(orgID, target)
			if err != nil {
				slog.Warn("otel routing: failed to build external exporter",
					slog.String("org_id", orgID), slog.String("endpoint", target.ExternalEndpoint), slog.Any("error", err))
				errs = append(errs, fmt.Errorf("org %s external exporter: %w", orgID, err))
				continue
			}
			if err := exp.ExportSpans(ctx, group); err != nil {
				slog.Warn("otel routing: external export failed",
					slog.String("org_id", orgID), slog.String("endpoint", target.ExternalEndpoint),
					slog.Int("spans", len(group)), slog.Any("error", err))
				errs = append(errs, fmt.Errorf("org %s external export: %w", orgID, err))
			} else {
				slog.Debug("otel routing: exported to external collector",
					slog.String("org_id", orgID), slog.String("endpoint", target.ExternalEndpoint), slog.Int("spans", len(group)))
			}
		}
	}

	if e.def != nil && len(defaultBatch) > 0 {
		if err := e.def.ExportSpans(ctx, defaultBatch); err != nil {
			errs = append(errs, fmt.Errorf("default export: %w", err))
		}
	}

	return errors.Join(errs...)
}

// exporterFor returns a cached OTLP exporter for the org, rebuilding it
// when the org's config changes (detected via a config hash).
func (e *routingExporter) exporterFor(orgID string, target *OrgExportTarget) (trace.SpanExporter, error) {
	h := targetHash(target)

	e.mu.Lock()
	defer e.mu.Unlock()

	if c, ok := e.cache[orgID]; ok {
		if c.hash == h {
			return c.exp, nil
		}
		// Config changed: drop the stale exporter.
		if err := c.exp.Shutdown(context.Background()); err != nil {
			slog.Warn("failed to shut down stale org exporter", slog.String("org_id", orgID), slog.Any("error", err))
		}
		delete(e.cache, orgID)
	}

	exp, err := buildExternalExporter(target)
	if err != nil {
		return nil, err
	}
	headerKeys := make([]string, 0, len(target.ExternalHeaders))
	for k := range target.ExternalHeaders {
		headerKeys = append(headerKeys, k)
	}
	slog.Info("otel routing: built external OTLP exporter",
		slog.String("org_id", orgID), slog.String("endpoint", target.ExternalEndpoint),
		slog.Bool("insecure", target.ExternalInsecure), slog.Any("header_keys", headerKeys))
	e.cache[orgID] = &cachedExporter{hash: h, exp: exp}
	return exp, nil
}

func (e *routingExporter) Shutdown(ctx context.Context) error {
	var errs []error
	if e.def != nil {
		if err := e.def.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	e.mu.Lock()
	for orgID, c := range e.cache {
		if err := c.exp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		delete(e.cache, orgID)
	}
	e.mu.Unlock()
	return errors.Join(errs...)
}

func spanOrgID(s trace.ReadOnlySpan) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == orgIDAttrKey {
			return kv.Value.AsString()
		}
	}
	return ""
}

func targetHash(t *OrgExportTarget) string {
	keys := make([]string, 0, len(t.ExternalHeaders))
	for k := range t.ExternalHeaders {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%t|", t.ExternalEndpoint, t.ExternalInsecure)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s;", k, t.ExternalHeaders[k])
	}
	return b.String()
}

// buildExternalExporter constructs an OTLP/HTTP exporter for an org's
// collector. A full URL is accepted: the scheme decides insecure (http),
// the host maps to WithEndpoint and any path to WithURLPath.
func buildExternalExporter(t *OrgExportTarget) (trace.SpanExporter, error) {
	host := t.ExternalEndpoint
	path := ""
	insecure := t.ExternalInsecure

	if u, err := url.Parse(t.ExternalEndpoint); err == nil && u.Host != "" {
		host = u.Host
		path = u.Path
		switch u.Scheme {
		case "http":
			insecure = true
		case "https":
			insecure = false
		}
	} else {
		// No scheme: strip a leading // if present.
		host = strings.TrimPrefix(host, "//")
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host)}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if path != "" && path != "/" {
		opts = append(opts, otlptracehttp.WithURLPath(path))
	}
	if len(t.ExternalHeaders) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(t.ExternalHeaders))
	}

	return otlptracehttp.New(context.Background(), opts...)
}
