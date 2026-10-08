package bench

import (
	"fmt"
	"testing"
)

var sizes = []int{1, 10, 50, 100}

func BenchmarkIngest(b *testing.B) {
	for _, n := range sizes {
		recs := Scenario(n)
		b.Run(fmt.Sprintf("actions=%d", n), func(b *testing.B) { Ingest(b, recs) })
	}
}

func BenchmarkIndexAndGraph(b *testing.B) {
	for _, n := range sizes {
		recs := Scenario(n)
		b.Run(fmt.Sprintf("actions=%d", n), func(b *testing.B) { IndexAndGraph(b, recs) })
	}
}

func BenchmarkQuery(b *testing.B) {
	for _, n := range sizes {
		recs := Scenario(n)
		b.Run(fmt.Sprintf("actions=%d", n), func(b *testing.B) { Query(b, recs) })
	}
}

func TestScenarioSeparatesConcurrentActions(t *testing.T) {
	// The benchmark workload must itself be correct: every concurrent
	// restart attributes exactly its own Deployment's objects.
	recs := Scenario(10)
	st, now := load(recs)
	_ = st
	e := newEngine(st)
	for _, a := range e.Actions(now, zero) {
		g, err := e.Graph(now, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Ambiguities) != 0 {
			t.Fatalf("%s: unexpected ambiguities", a.ID)
		}
		target := g.Action.Targets[0].Name
		for _, n := range g.Nodes {
			if n.Object != nil && n.Object.Kind != "Deployment" && len(n.Object.Name) >= len(target) && n.Object.Name[:len(target)] != target {
				t.Fatalf("%s attached %s", target, n.Object.Name)
			}
		}
	}
}
