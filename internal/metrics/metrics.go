// Package metrics exposes EffectTrace's own Prometheus metrics.
//
// Every label is bounded: record kind, ingest result, evidence type, graph
// status, action kind and source. Graph IDs, trace IDs, span IDs and object
// UIDs are never used as labels.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/effecttrace/effecttrace/internal/obs"
)

// Metrics holds the collector's instruments.
type Metrics struct {
	Registry      *prometheus.Registry
	ingest        *prometheus.CounterVec
	GraphBuild    prometheus.Histogram
	Telemetry     *prometheus.CounterVec
	Graphs        *prometheus.GaugeVec
	Actions       *prometheus.GaugeVec
	Edges         *prometheus.GaugeVec
	Ambiguities   prometheus.Gauge
	ExportedGraph *prometheus.CounterVec
}

// New registers all instruments. queueDepth reports the pipeline depth.
func New(queueDepth, queueCapacity func() float64, otlp func(result string) float64) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		ingest: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "effecttrace_ingest_records_total", Help: "Observation records processed, by kind and result (applied, duplicate, rejected, throttled).",
		}, []string{"kind", "result"}),
		GraphBuild: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "effecttrace_graph_build_duration_seconds", Help: "Time to build one effect graph.",
			Buckets: []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 1},
		}),
		Telemetry: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "effecttrace_telemetry_evaluations_total", Help: "Signal evaluations, by result (changed, unchanged, nodata, error).",
		}, []string{"result"}),
		Graphs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "effecttrace_graphs", Help: "Retained effect graphs, by status.",
		}, []string{"status"}),
		Actions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "effecttrace_actions", Help: "Retained actions, by kind.",
		}, []string{"kind"}),
		Edges: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "effecttrace_graph_edges", Help: "Edges across retained effect graphs, by evidence type.",
		}, []string{"evidence_type"}),
		Ambiguities: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "effecttrace_graph_ambiguities", Help: "Changes left unattributed across retained graphs because more than one action claimed them.",
		}),
		ExportedGraph: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "effecttrace_otlp_exported_graphs_total", Help: "Completed graphs exported as OTLP spans, by result.",
		}, []string{"result"}),
	}
	reg.MustRegister(m.ingest, m.GraphBuild, m.Telemetry, m.Graphs, m.Actions, m.Edges, m.Ambiguities, m.ExportedGraph,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "effecttrace_queue_depth", Help: "Records waiting in the ingest queue."}, queueDepth),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "effecttrace_queue_capacity", Help: "Ingest queue capacity."}, queueCapacity),
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	for _, result := range []string{"accepted", "rejected", "throttled"} {
		r := result
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "effecttrace_otlp_spans_total", Help: "OTLP spans received, by result.", ConstLabels: prometheus.Labels{"result": r},
		}, func() float64 { return otlp(r) }))
	}
	return m
}

// Ingested implements pipeline.Observer.
func (m *Metrics) Ingested(kind obs.Kind, result string) {
	m.ingest.WithLabelValues(string(kind), result).Inc()
}
