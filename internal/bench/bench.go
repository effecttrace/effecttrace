// Package bench contains EffectTrace's synthetic, in-process benchmarks. They
// use generated observation streams (internal/synth), not a cluster, and
// measure the correlation engine alone. They are not end-to-end or
// production-scale measurements.
package bench

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/internal/synth"
	"github.com/effecttrace/effecttrace/pkg/model"
)

// Scenario generates n concurrent agent restarts, each on its own
// Deployment with three Pods, and returns the records.
func Scenario(n int) []obs.Record {
	c := synth.New(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	c.Sources()
	ws := make([]*synth.Workload, n)
	for i := range n {
		ws[i] = c.Deployment("bench", fmt.Sprintf("svc-%03d", i), 3)
	}
	c.Advance(time.Minute)
	for _, w := range ws {
		_, finish := c.MCPCall("restart_workload", synth.Request{Verb: "patch", Res: "deployments", NS: "bench", Name: w.Deploy.Ref.Name}, w.Deploy, synth.MCPOptions{})
		w.Mutate()
		finish()
	}
	for _, w := range ws {
		w.Rollout(false)
	}
	return c.Records()
}

func load(recs []obs.Record) (*store.Store, time.Time) {
	st := store.New(store.DefaultConfig())
	for _, r := range recs {
		_, _ = st.Apply(r)
	}
	var now time.Time
	st.Read(func(v store.View) { now = v.Newest().Add(10 * time.Minute) })
	return st, now
}

// Ingest measures applying records to a fresh store.
func Ingest(b *testing.B, recs []obs.Record) {
	b.ReportAllocs()
	for b.Loop() {
		st := store.New(store.DefaultConfig())
		for _, r := range recs {
			_, _ = st.Apply(r)
		}
	}
	b.ReportMetric(float64(len(recs))*float64(b.N)/b.Elapsed().Seconds(), "records/s")
}

// IndexAndGraph measures discovering all actions and building one graph
// from a cold engine (what the first query after new data costs).
func IndexAndGraph(b *testing.B, recs []obs.Record) {
	st, now := load(recs)
	b.ReportAllocs()
	for b.Loop() {
		e := correlate.NewEngine(st, correlate.DefaultConfig(), correlate.Options{})
		acts := e.Actions(now, time.Time{})
		if _, err := e.Graph(now, acts[0].ID); err != nil {
			b.Fatal(err)
		}
	}
}

// Query measures building one graph with a warm action index (what repeated
// API queries cost while no new data arrives).
func Query(b *testing.B, recs []obs.Record) {
	st, now := load(recs)
	e := correlate.NewEngine(st, correlate.DefaultConfig(), correlate.Options{})
	acts := e.Actions(now, time.Time{})
	id := acts[len(acts)/2].ID
	b.ReportAllocs()
	for b.Loop() {
		g, err := e.Graph(now, id)
		if err != nil || g.Status != model.StatusComplete {
			b.Fatal(err)
		}
	}
}

// MemoryPerAction returns retained heap bytes per action after loading recs
// that contain n actions.
func MemoryPerAction(recs []obs.Record, n int) float64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	st, _ := load(recs)
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(st)
	return float64(after.HeapAlloc-before.HeapAlloc) / float64(n)
}

var zero time.Time

func newEngine(st *store.Store) *correlate.Engine {
	return correlate.NewEngine(st, correlate.DefaultConfig(), correlate.Options{})
}
