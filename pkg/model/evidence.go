// Package model defines the EffectTrace effect graph: an initiating Action,
// the nodes observed after it, and evidence-graded edges between them.
//
// The model deliberately separates the relationship an edge asserts from the
// class of evidence that supports it. A relationship type admits exactly one
// evidence class (see Relationship.Evidence), so temporal correlation can
// never be presented as direct or structural evidence.
package model

import "fmt"

// SchemaVersion identifies the canonical JSON encoding of an EffectGraph.
const SchemaVersion = "effecttrace.io/v1alpha1"

// EvidenceType is the class of evidence that supports an edge.
type EvidenceType string

const (
	// EvidenceDirect means the initiating operation itself performed the
	// mutation, verified by request-level identifiers (for example the
	// kube-apiserver Audit-ID or the object UID and resourceVersion returned
	// to the instrumented client).
	EvidenceDirect EvidenceType = "DIRECT"
	// EvidenceStructural means Kubernetes object structure (controller
	// ownerReferences, matched by UID) establishes the relationship.
	EvidenceStructural EvidenceType = "STRUCTURAL"
	// EvidenceTraceLink means an OpenTelemetry parent/child or span link
	// relationship establishes the relationship.
	EvidenceTraceLink EvidenceType = "TRACE_LINK"
	// EvidenceEventReference means a Kubernetes Event explicitly references
	// the object by UID.
	EvidenceEventReference EvidenceType = "EVENT_REFERENCE"
	// EvidenceTemporalCorrelation means the observation happened inside the
	// configured observation window. Causation is NOT established.
	EvidenceTemporalCorrelation EvidenceType = "TEMPORAL_CORRELATION"
	// EvidenceInferred is reserved for experimental heuristics. EffectTrace
	// v0.1 does not emit it.
	EvidenceInferred EvidenceType = "INFERRED"
)

// AllEvidenceTypes lists evidence classes in a stable display order.
var AllEvidenceTypes = []EvidenceType{
	EvidenceDirect,
	EvidenceTraceLink,
	EvidenceStructural,
	EvidenceEventReference,
	EvidenceTemporalCorrelation,
	EvidenceInferred,
}

// Valid reports whether e is a known evidence class.
func (e EvidenceType) Valid() bool {
	for _, k := range AllEvidenceTypes {
		if e == k {
			return true
		}
	}
	return false
}

// Attributable reports whether the evidence class identifies a relationship
// from authoritative identifiers rather than from timing alone.
func (e EvidenceType) Attributable() bool {
	switch e {
	case EvidenceDirect, EvidenceStructural, EvidenceTraceLink, EvidenceEventReference:
		return true
	default:
		return false
	}
}

// Relationship is what an edge asserts about its two nodes.
type Relationship string

const (
	// RelDirectRequest connects an action or request to the object that the
	// request mutated.
	RelDirectRequest Relationship = "DIRECT_REQUEST"
	// RelStructuralOwner connects a controller owner to an object it owns.
	RelStructuralOwner Relationship = "STRUCTURAL_OWNER"
	// RelTraceParent connects a span to a descendant span in the same trace.
	RelTraceParent Relationship = "TRACE_PARENT"
	// RelTraceLink connects spans related by an OpenTelemetry span link.
	RelTraceLink Relationship = "TRACE_LINK"
	// RelEventReference connects an object to a Kubernetes Event that
	// references it.
	RelEventReference Relationship = "EVENT_REFERENCE"
	// RelTemporalCorrelation connects an observation to the node whose
	// observation window it fell in. It never implies causation.
	RelTemporalCorrelation Relationship = "TEMPORAL_CORRELATION"
)

// AllRelationships lists relationships in a stable order.
var AllRelationships = []Relationship{
	RelTraceParent,
	RelTraceLink,
	RelDirectRequest,
	RelStructuralOwner,
	RelEventReference,
	RelTemporalCorrelation,
}

// Evidence returns the only evidence class a relationship may carry.
func (r Relationship) Evidence() (EvidenceType, error) {
	switch r {
	case RelDirectRequest:
		return EvidenceDirect, nil
	case RelStructuralOwner:
		return EvidenceStructural, nil
	case RelTraceParent, RelTraceLink:
		return EvidenceTraceLink, nil
	case RelEventReference:
		return EvidenceEventReference, nil
	case RelTemporalCorrelation:
		return EvidenceTemporalCorrelation, nil
	default:
		return "", fmt.Errorf("unknown relationship %q", string(r))
	}
}

// Valid reports whether r is a known relationship.
func (r Relationship) Valid() bool {
	_, err := r.Evidence()
	return err == nil
}

// PathGrade summarizes the weakest evidence on the path from the action to a
// node. A path is only as strong as its weakest edge.
type PathGrade string

const (
	// GradeAttributed means every edge on the path is DIRECT, TRACE_LINK,
	// STRUCTURAL or EVENT_REFERENCE.
	GradeAttributed PathGrade = "ATTRIBUTED"
	// GradeCorrelated means at least one edge on the path is
	// TEMPORAL_CORRELATION.
	GradeCorrelated PathGrade = "CORRELATED"
	// GradeInferred means at least one edge on the path is INFERRED.
	GradeInferred PathGrade = "INFERRED"
)

// gradeRank orders path grades from strongest to weakest.
func gradeRank(g PathGrade) int {
	switch g {
	case GradeAttributed:
		return 0
	case GradeCorrelated:
		return 1
	default:
		return 2
	}
}

// GradeOf returns the path grade contributed by a single evidence class.
func GradeOf(e EvidenceType) PathGrade {
	switch {
	case e.Attributable():
		return GradeAttributed
	case e == EvidenceTemporalCorrelation:
		return GradeCorrelated
	default:
		return GradeInferred
	}
}

// Weaker returns the weaker of two grades.
func Weaker(a, b PathGrade) PathGrade {
	if gradeRank(a) >= gradeRank(b) {
		return a
	}
	return b
}
