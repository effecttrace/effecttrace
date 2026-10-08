// Package otelexport emits completed effect graphs as OpenTelemetry spans.
//
// Each graph becomes one span in a NEW trace that carries a span LINK to the
// initiating tool span. Parent/child is deliberately not used: the controller
// effects are asynchronous, the tool span does not enclose them, and the
// OpenTelemetry specification recommends links for that case. Edges are
// recorded as span events.
package otelexport

import (
	"context"
	"encoding/hex"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/effecttrace/effecttrace/pkg/model"
	"github.com/effecttrace/effecttrace/pkg/semconv"
)

// MaxEdgeEvents bounds the number of edge events per exported span.
const MaxEdgeEvents = 128

// Exporter exports graphs.
type Exporter struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
}

// New creates an exporter that sends OTLP/HTTP to endpoint (host:port).
func New(ctx context.Context, endpoint string, insecure bool, version string) (*Exporter, error) {
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return NewWithSpanExporter(exp, version), nil
}

// NewWithSpanExporter wraps any span exporter (used by tests).
func NewWithSpanExporter(exp sdktrace.SpanExporter, version string) *Exporter {
	res := resource.NewSchemaless(attribute.String(semconv.ServiceName, "effecttrace"), attribute.String("service.version", version))
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	return &Exporter{tp: tp, tracer: tp.Tracer("github.com/effecttrace/effecttrace")}
}

// Shutdown flushes and stops the exporter.
func (e *Exporter) Shutdown(ctx context.Context) error { return e.tp.Shutdown(ctx) }

// ForceFlush exports buffered spans.
func (e *Exporter) ForceFlush(ctx context.Context) error { return e.tp.ForceFlush(ctx) }

func spanContext(ref *model.TraceRef) (trace.SpanContext, bool) {
	if ref == nil {
		return trace.SpanContext{}, false
	}
	var tid trace.TraceID
	var sid trace.SpanID
	tb, err1 := hex.DecodeString(ref.TraceID)
	sb, err2 := hex.DecodeString(ref.SpanID)
	if err1 != nil || err2 != nil || len(tb) != 16 || len(sb) != 8 {
		return trace.SpanContext{}, false
	}
	copy(tid[:], tb)
	copy(sid[:], sb)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true})
	return sc, sc.IsValid()
}

// Export records one graph.
func (e *Exporter) Export(ctx context.Context, g *model.EffectGraph) {
	start := g.Action.StartedAt
	end := start
	for _, w := range g.Windows {
		if w.End.After(end) {
			end = w.End
		}
	}
	counts := map[model.EvidenceType]int{}
	for _, ed := range g.Edges {
		counts[ed.Evidence]++
	}
	attrs := []attribute.KeyValue{
		attribute.String(semconv.GraphID, g.ID),
		attribute.String(semconv.GraphStatus, string(g.Status)),
		attribute.String(semconv.ActionID, g.Action.ID),
		attribute.String(semconv.ActionKind, string(g.Action.Kind)),
	}
	for _, et := range model.AllEvidenceTypes {
		attrs = append(attrs, attribute.Int(semconv.EdgeCountPrefix+strings.ToLower(string(et)), counts[et]))
	}
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(), trace.WithTimestamp(start), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...),
	}
	if sc, ok := spanContext(g.Action.Trace); ok {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc, Attributes: []attribute.KeyValue{
			attribute.String("effecttrace.link.type", "initiating_action"),
		}}))
	}
	_, span := e.tracer.Start(ctx, "effecttrace effect_graph", opts...)
	nodes := map[string]model.Node{}
	for _, n := range g.Nodes {
		nodes[n.ID] = n
	}
	for i, ed := range g.Edges {
		if i >= MaxEdgeEvents {
			span.SetAttributes(attribute.Int("effecttrace.edges.truncated", len(g.Edges)-MaxEdgeEvents))
			break
		}
		ea := []attribute.KeyValue{
			attribute.String(semconv.EdgeRelationship, string(ed.Relationship)),
			attribute.String(semconv.EvidenceType, string(ed.Evidence)),
			attribute.String("effecttrace.edge.from", ed.From),
			attribute.String("effecttrace.edge.to", ed.To),
			attribute.String(semconv.EdgeReason, truncate(ed.Reason, 256)),
		}
		if n, ok := nodes[ed.To]; ok {
			ea = append(ea, attribute.String(semconv.NodeType, string(n.Type)))
			if n.Object != nil && n.Object.UID != "" {
				ea = append(ea, attribute.String(semconv.K8sUID, n.Object.UID))
			}
		}
		ts := ed.ObservedAt
		if ts.IsZero() || ts.Before(start) {
			ts = start
		}
		span.AddEvent("effecttrace.edge", trace.WithTimestamp(ts), trace.WithAttributes(ea...))
	}
	if g.Status != model.StatusComplete {
		span.SetStatus(codes.Unset, "")
	}
	if end.Before(start) {
		end = start.Add(time.Millisecond)
	}
	span.End(trace.WithTimestamp(end))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
