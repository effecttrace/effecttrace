package render_test

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/render"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/internal/synth"
	"github.com/effecttrace/effecttrace/pkg/model"
)

func restartGraph(t *testing.T) *model.EffectGraph {
	t.Helper()
	c := synth.New(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	c.Sources()
	w := c.Deployment("shop", "checkout", 3)
	other := c.Deployment("shop", "payments", 2)
	c.Advance(time.Minute)
	_, finish := c.MCPCall("restart_workload", synth.Request{Verb: "patch", Res: "deployments", NS: "shop", Name: "checkout"}, w.Deploy, synth.MCPOptions{})
	w.Mutate()
	finish()
	c.Advance(100 * time.Millisecond)
	c.Audit(synth.Request{AuditID: "human-1", Verb: "patch", Res: "deployments", NS: "shop", Name: "payments", User: "kubernetes-admin", At: c.Now})
	other.Mutate()
	other.Scale(3)
	w.Rollout(false)
	st := store.New(store.DefaultConfig())
	for _, r := range c.Records() {
		st.Apply(r)
	}
	// Telemetry results for the action, as the evaluator would record them.
	e := correlate.NewEngine(st, correlate.DefaultConfig(), correlate.Options{TelemetryConfigured: true})
	now := c.Now.Add(10 * time.Minute)
	var id string
	for _, a := range e.Actions(now, time.Time{}) {
		if a.Kind == model.ActionMCPToolCall {
			id = a.ID
		}
	}
	for _, tt := range e.PendingTelemetry(now) {
		if tt.ActionID != id {
			continue
		}
		for _, wl := range append(tt.InScope, tt.Others...) {
			changed := wl.Name == "checkout" || wl.Name == "payments"
			st.Apply(obs.Record{Kind: obs.KindMetric, Metric: &obs.MetricResult{ActionID: id, Signal: "http_error_ratio", Unit: "ratio",
				Namespace: "shop", Workload: wl.Name, WorkloadUID: wl.UID, BaselineStart: tt.BaselineStart, WindowStart: tt.WindowStart,
				WindowEnd: tt.WindowEnd, Baseline: 0.001, Observed: 0.042, Samples: 12, Changed: changed, Direction: "increased"}})
		}
	}
	g, err := e.Graph(now, id)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestExplainShowsEvidenceClasses(t *testing.T) {
	g := restartGraph(t)
	var buf bytes.Buffer
	render.Explain(&buf, g, render.Options{})
	out := buf.String()
	if os.Getenv("EFFECTTRACE_SHOW") != "" {
		t.Log("\n" + out)
	}
	for _, want := range []string{
		"ACTION  tools/call restart_workload",
		"TRACE LINK", "DIRECT", "STRUCTURAL", "EVENT REFERENCE", "TEMPORAL CORRELATION",
		"3 Pods created", "3 Pods terminated",
		"EXCLUDED", "payments",
		"Temporal correlation does not prove causation.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain output lacks %q", want)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("color codes emitted with Color=false")
	}
}

func TestRenderersEscapeHostileLabels(t *testing.T) {
	g := restartGraph(t)
	g.Nodes[0].Label = "evil\x1b[2J\"<script>alert(1)</script>"
	var buf bytes.Buffer
	render.Explain(&buf, g, render.Options{})
	render.DOT(&buf, g)
	render.Mermaid(&buf, g)
	out := buf.String()
	if strings.Contains(out, "\x1b[2J") {
		t.Error("terminal escape survived rendering")
	}
	if strings.Contains(out, "<script>") && strings.Contains(out, "flowchart") && strings.Contains(out[strings.Index(out, "flowchart"):], "<script>") {
		t.Error("mermaid output contains raw HTML")
	}
}
