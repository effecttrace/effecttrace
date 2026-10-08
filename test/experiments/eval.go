package experiments

import (
	"slices"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/pkg/model"
)

// Counts are confusion counts for one claim class.
type Counts struct {
	TP int `json:"tp"`
	FP int `json:"fp"`
	FN int `json:"fn"`
}

// Add accumulates counts.
func (c *Counts) Add(o Counts) { c.TP += o.TP; c.FP += o.FP; c.FN += o.FN }

// Precision returns TP/(TP+FP), or nil when there were no claims.
func (c Counts) Precision() *float64 {
	if c.TP+c.FP == 0 {
		return nil
	}
	v := float64(c.TP) / float64(c.TP+c.FP)
	return &v
}

// Recall returns TP/(TP+FN), or nil when nothing was expected.
func (c Counts) Recall() *float64 {
	if c.TP+c.FN == 0 {
		return nil
	}
	v := float64(c.TP) / float64(c.TP+c.FN)
	return &v
}

// Evaluation compares one graph with its ground truth.
type Evaluation struct {
	Direct     Counts `json:"direct"`
	Structural Counts `json:"structural"`
	// FalseAttachments are attributed claims on objects known to be
	// unrelated to this action.
	FalseAttachments []string `json:"falseAttachments"`
	// AmbiguousClaimed are attributed claims on changes the ground truth
	// marks as ambiguous between actions.
	AmbiguousClaimed []string `json:"ambiguousClaimed"`
	// UnrelatedTelemetry lists TEMPORAL_CORRELATION edges to signals of
	// workloads outside the action's telemetry scope.
	UnrelatedTelemetry []string `json:"unrelatedTelemetry"`
	// Claims are all attributed object UIDs (for concurrency checks).
	Claims []string `json:"-"`
	// Correlated counts objects reachable only through temporal edges.
	Correlated  int            `json:"correlatedObjects"`
	EdgeCounts  map[string]int `json:"edgeCounts"`
	Ambiguities int            `json:"ambiguities"`
	Exclusions  int            `json:"exclusions"`
}

func incoming(g *model.EffectGraph) map[string][]model.Edge {
	in := map[string][]model.Edge{}
	for _, e := range g.Edges {
		in[e.To] = append(in[e.To], e)
	}
	return in
}

// Evaluate scores g against t. unrelated are UIDs that changed in the
// scenario but belong to no expectation of this action.
func Evaluate(g *model.EffectGraph, t Truth, unrelated []string) Evaluation {
	ev := Evaluation{EdgeCounts: map[string]int{}, Ambiguities: len(g.Ambiguities), Exclusions: len(g.Exclusions),
		FalseAttachments: []string{}, AmbiguousClaimed: []string{}, UnrelatedTelemetry: []string{}}
	for _, e := range g.Edges {
		ev.EdgeCounts[string(e.Evidence)]++
	}
	in := incoming(g)
	var direct, structural []string
	for _, n := range g.Nodes {
		if n.Object == nil || n.Object.UID == "" || n.Change == nil {
			continue
		}
		if n.Grade == model.GradeCorrelated {
			ev.Correlated++
			continue
		}
		if n.Grade != model.GradeAttributed {
			continue
		}
		uid := n.Object.UID
		ev.Claims = append(ev.Claims, uid)
		for _, e := range in[n.ID] {
			switch e.Evidence {
			case model.EvidenceDirect:
				direct = append(direct, uid)
			case model.EvidenceStructural:
				// An ownership context edge (for example ReplicaSet -> the
				// Pod a tool deleted) explains structure; it is not a claim
				// that the action caused a change through the controller.
				if e.Rule != correlate.RuleOwnerContext {
					structural = append(structural, uid)
				}
			}
		}
		if slices.Contains(unrelated, uid) {
			ev.FalseAttachments = append(ev.FalseAttachments, n.Label)
		}
		if slices.Contains(t.Ambiguous, uid) && !slices.Contains(t.Uncertain, uid) {
			ev.AmbiguousClaimed = append(ev.AmbiguousClaimed, n.Label)
		}
	}
	ev.Direct = score(direct, t.Direct, nil)
	ev.Structural = score(structural, t.Structural, t.Uncertain)
	nodes := map[string]model.Node{}
	for _, n := range g.Nodes {
		nodes[n.ID] = n
	}
	for _, e := range g.Edges {
		if e.Evidence != model.EvidenceTemporalCorrelation {
			continue
		}
		n := nodes[e.To]
		if n.Metric != nil && !slices.Contains(t.Workloads, n.Metric.Workload) {
			ev.UnrelatedTelemetry = append(ev.UnrelatedTelemetry, n.Label)
		}
	}
	return ev
}

func score(claimed, expected, uncertain []string) Counts {
	var c Counts
	claimed = uniq(claimed)
	for _, u := range claimed {
		if slices.Contains(uncertain, u) && !slices.Contains(expected, u) {
			continue
		}
		if slices.Contains(expected, u) {
			c.TP++
		} else {
			c.FP++
		}
	}
	for _, u := range expected {
		if !slices.Contains(claimed, u) {
			c.FN++
		}
	}
	return c
}

func uniq(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}
