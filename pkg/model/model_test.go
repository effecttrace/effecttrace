package model

import (
	"errors"
	"testing"
	"time"
)

func sample() *EffectGraph {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	g := &EffectGraph{SchemaVersion: SchemaVersion, ID: "g-a", Status: StatusComplete,
		Action: Action{ID: "a", Kind: ActionMCPToolCall, Name: "tools/call restart_workload", StartedAt: t0}}
	g.Nodes = []Node{
		{ID: ActionNodeID("a"), Type: NodeAction, Label: "tools/call restart_workload"},
		{ID: "request:1", Type: NodeKubernetesRequest, Label: "PATCH deployments shop/checkout"},
		{ID: "k8s:d", Type: NodeKubernetesObject, Label: "Deployment shop/checkout", Object: &ObjectRef{Kind: "Deployment", Name: "checkout", UID: "d"}},
		{ID: "k8s:rs", Type: NodeKubernetesObject, Label: "ReplicaSet", Object: &ObjectRef{Kind: "ReplicaSet", Name: "checkout-1", UID: "rs"}},
		{ID: "metric:p99:d", Type: NodeMetricObservation, Label: "p99"},
		{ID: "k8s:p", Type: NodeKubernetesObject, Label: "Pod"},
	}
	add := func(from, to string, rel Relationship) {
		e, err := NewEdge(from, to, rel, "r", "test", t0)
		if err != nil {
			panic(err)
		}
		g.Edges = append(g.Edges, e)
	}
	add(ActionNodeID("a"), "request:1", RelTraceParent)
	add("request:1", "k8s:d", RelDirectRequest)
	add("k8s:d", "k8s:rs", RelStructuralOwner)
	add("k8s:d", "metric:p99:d", RelTemporalCorrelation)
	add("metric:p99:d", "k8s:p", RelStructuralOwner) // pathological, for grading
	return g
}

func TestRelationshipEvidenceIsFixed(t *testing.T) {
	want := map[Relationship]EvidenceType{
		RelDirectRequest: EvidenceDirect, RelStructuralOwner: EvidenceStructural, RelTraceParent: EvidenceTraceLink,
		RelTraceLink: EvidenceTraceLink, RelEventReference: EvidenceEventReference, RelTemporalCorrelation: EvidenceTemporalCorrelation,
	}
	for rel, ev := range want {
		got, err := rel.Evidence()
		if err != nil || got != ev {
			t.Errorf("%s -> %s, %v; want %s", rel, got, err, ev)
		}
	}
	if _, err := Relationship("RELATED_TO").Evidence(); err == nil {
		t.Error("unknown relationship accepted")
	}
	if EvidenceTemporalCorrelation.Attributable() || EvidenceInferred.Attributable() {
		t.Error("weak evidence reported as attributable")
	}
}

func TestValidateRejectsUpgradedEvidence(t *testing.T) {
	g := sample()
	for i := range g.Edges {
		if g.Edges[i].Relationship == RelTemporalCorrelation {
			g.Edges[i].Evidence = EvidenceDirect
		}
	}
	if err := g.Validate(); !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("Validate = %v, want ErrEvidenceMismatch", err)
	}
}

func TestValidateDetectsStructuralErrors(t *testing.T) {
	g := sample()
	g.Nodes = append(g.Nodes, g.Nodes[1])
	if err := g.Validate(); !errors.Is(err, ErrDuplicateNode) {
		t.Errorf("duplicate: %v", err)
	}
	g = sample()
	g.Edges[0].To = "missing"
	if err := g.Validate(); !errors.Is(err, ErrDanglingEdge) {
		t.Errorf("dangling: %v", err)
	}
	g = sample()
	g.Nodes = g.Nodes[1:]
	if err := g.Validate(); !errors.Is(err, ErrMissingActionNode) {
		t.Errorf("missing action: %v", err)
	}
	g = sample()
	g.Edges[0].ID = "e-forged"
	if err := g.Validate(); !errors.Is(err, ErrInvalidValue) {
		t.Errorf("forged edge id: %v", err)
	}
}

func TestGradesUseWeakestEdgeOnStrongestPath(t *testing.T) {
	g := sample()
	g.Canonicalize()
	grade := map[string]PathGrade{}
	for _, n := range g.Nodes {
		grade[n.ID] = n.Grade
	}
	if grade["k8s:rs"] != GradeAttributed {
		t.Errorf("rs = %s", grade["k8s:rs"])
	}
	if grade["metric:p99:d"] != GradeCorrelated {
		t.Errorf("metric = %s", grade["metric:p99:d"])
	}
	if grade["k8s:p"] != GradeCorrelated {
		t.Errorf("pod reached only through a correlated edge = %s", grade["k8s:p"])
	}
}

func TestCanonicalRoundTrip(t *testing.T) {
	g := sample()
	b1, err := g.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	g2, err := UnmarshalGraph(b1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := g2.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatal("canonical encoding is not stable across a round trip")
	}
}

func TestUnmarshalRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	b, _ := sample().MarshalCanonical()
	if _, err := UnmarshalGraph(append([]byte(`{"evil":1,`), b[1:]...)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := UnmarshalGraph(append(b, []byte(`{}`)...)); err == nil {
		t.Error("trailing data accepted")
	}
}

func FuzzUnmarshalGraph(f *testing.F) {
	b, _ := sample().MarshalCanonical()
	f.Add(b)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schemaVersion":"effecttrace.io/v1alpha1","id":"x","action":{"id":"a"},"nodes":[{"id":"action:a","type":"ACTION"}],"edges":[{"from":"action:a","to":"action:a","relationship":"TEMPORAL_CORRELATION","evidence":"DIRECT"}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		g, err := UnmarshalGraph(data)
		if err != nil {
			return
		}
		// Anything accepted must be valid, re-encodable and stable.
		if err := g.Validate(); err != nil {
			t.Fatalf("accepted invalid graph: %v", err)
		}
		for _, e := range g.Edges {
			want, _ := e.Relationship.Evidence()
			if e.Evidence != want {
				t.Fatalf("accepted mismatched evidence %s/%s", e.Relationship, e.Evidence)
			}
		}
		b1, err := g.MarshalCanonical()
		if err != nil {
			t.Fatal(err)
		}
		g2, err := UnmarshalGraph(b1)
		if err != nil {
			t.Fatal(err)
		}
		b2, _ := g2.MarshalCanonical()
		if string(b1) != string(b2) {
			t.Fatal("unstable canonical form")
		}
	})
}
