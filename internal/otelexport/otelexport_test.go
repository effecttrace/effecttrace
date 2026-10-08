package otelexport

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/effecttrace/effecttrace/pkg/model"
	"github.com/effecttrace/effecttrace/pkg/semconv"
)

func TestExportUsesLinkNotParent(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	e := NewWithSpanExporter(exp, "test")
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	g := &model.EffectGraph{SchemaVersion: model.SchemaVersion, ID: "g-a", Status: model.StatusComplete,
		Action:  model.Action{ID: "a", Kind: model.ActionMCPToolCall, StartedAt: t0, Trace: &model.TraceRef{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7"}},
		Windows: []model.ObservationWindow{{Name: "reconcile", Start: t0, End: t0.Add(10 * time.Second)}},
		Nodes:   []model.Node{{ID: "action:a", Type: model.NodeAction}, {ID: "k8s:u", Type: model.NodeKubernetesObject, Object: &model.ObjectRef{UID: "u"}}}}
	ed, _ := model.NewEdge("action:a", "k8s:u", model.RelDirectRequest, "r", "otlp", t0)
	g.Edges = []model.Edge{ed}
	e.Export(context.Background(), g)
	if err := e.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	s := spans[0]
	if s.Parent.IsValid() {
		t.Error("graph span must not be a child of the tool span")
	}
	if len(s.Links) != 1 || s.Links[0].SpanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("links = %+v", s.Links)
	}
	if s.SpanContext.TraceID() == s.Links[0].SpanContext.TraceID() {
		t.Error("graph span should start a new trace")
	}
	if len(s.Events) != 1 || !s.EndTime.Equal(t0.Add(10*time.Second)) {
		t.Errorf("events=%d end=%v", len(s.Events), s.EndTime)
	}
	found := false
	for _, a := range s.Attributes {
		if string(a.Key) == semconv.GraphID && a.Value.AsString() == "g-a" {
			found = true
		}
		if len(a.Key) >= 5 && string(a.Key[:5]) == "otel." {
			t.Errorf("reserved otel.* attribute used: %s", a.Key)
		}
	}
	if !found {
		t.Error("graph id attribute missing")
	}
}
