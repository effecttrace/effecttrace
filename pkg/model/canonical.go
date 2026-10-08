package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Validation errors.
var (
	ErrDuplicateNode     = errors.New("duplicate node id")
	ErrDanglingEdge      = errors.New("edge references unknown node")
	ErrEvidenceMismatch  = errors.New("edge evidence does not match relationship")
	ErrMissingActionNode = errors.New("graph has no action node")
	ErrInvalidValue      = errors.New("invalid value")
)

// EdgeID returns the deterministic identifier of an edge.
func EdgeID(from, to string, rel Relationship) string {
	h := sha256.Sum256([]byte(from + "\x00" + to + "\x00" + string(rel)))
	return "e-" + hex.EncodeToString(h[:8])
}

// NewEdge constructs an edge whose evidence class is derived from the
// relationship, so callers cannot pair a weak relationship with strong
// evidence.
func NewEdge(from, to string, rel Relationship, reason, source string, at time.Time) (Edge, error) {
	ev, err := rel.Evidence()
	if err != nil {
		return Edge{}, err
	}
	return Edge{
		ID:           EdgeID(from, to, rel),
		From:         from,
		To:           to,
		Relationship: rel,
		Evidence:     ev,
		Reason:       reason,
		Source:       source,
		ObservedAt:   at.UTC(),
	}, nil
}

// ActionNodeID returns the node identifier for an action.
func ActionNodeID(actionID string) string { return "action:" + actionID }

// Validate checks structural invariants of a graph.
func (g *EffectGraph) Validate() error {
	if g.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schemaVersion %q", ErrInvalidValue, g.SchemaVersion)
	}
	if g.Action.ID == "" {
		return fmt.Errorf("%w: empty action id", ErrInvalidValue)
	}
	nodes := make(map[string]NodeType, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.ID == "" {
			return fmt.Errorf("%w: empty node id", ErrInvalidValue)
		}
		if _, dup := nodes[n.ID]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateNode, n.ID)
		}
		nodes[n.ID] = n.Type
	}
	if nodes[ActionNodeID(g.Action.ID)] != NodeAction {
		return ErrMissingActionNode
	}
	for _, e := range g.Edges {
		if _, ok := nodes[e.From]; !ok {
			return fmt.Errorf("%w: from %s", ErrDanglingEdge, e.From)
		}
		if _, ok := nodes[e.To]; !ok {
			return fmt.Errorf("%w: to %s", ErrDanglingEdge, e.To)
		}
		want, err := e.Relationship.Evidence()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidValue, err)
		}
		if e.Evidence != want {
			return fmt.Errorf("%w: %s carries %s, want %s", ErrEvidenceMismatch, e.Relationship, e.Evidence, want)
		}
		if e.ID != EdgeID(e.From, e.To, e.Relationship) {
			return fmt.Errorf("%w: edge id %s", ErrInvalidValue, e.ID)
		}
	}
	return nil
}

// Canonicalize sorts all collections, normalizes timestamps to UTC and
// recomputes node grades so that equal graphs encode to identical bytes.
func (g *EffectGraph) Canonicalize() {
	g.SchemaVersion = SchemaVersion
	g.Action.StartedAt = g.Action.StartedAt.UTC()
	g.Action.EndedAt = g.Action.EndedAt.UTC()
	slices.SortFunc(g.Action.Targets, compareObjectRef)
	for i := range g.Windows {
		g.Windows[i].Start = g.Windows[i].Start.UTC()
		g.Windows[i].End = g.Windows[i].End.UTC()
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		n.ObservedAt = n.ObservedAt.UTC()
		if n.Change != nil {
			n.Change.At = n.Change.At.UTC()
			n.Change.ReadyAt = n.Change.ReadyAt.UTC()
			n.Change.DeletedAt = n.Change.DeletedAt.UTC()
		}
		if n.Request != nil {
			n.Request.ReceivedAt = n.Request.ReceivedAt.UTC()
			slices.Sort(n.Request.Sources)
			n.Request.Sources = slices.Compact(n.Request.Sources)
		}
	}
	slices.SortFunc(g.Nodes, func(a, b Node) int { return strings.Compare(a.ID, b.ID) })
	for i := range g.Edges {
		e := &g.Edges[i]
		e.ObservedAt = e.ObservedAt.UTC()
		slices.SortFunc(e.Facts, func(a, b Fact) int {
			if c := strings.Compare(a.Key, b.Key); c != 0 {
				return c
			}
			return strings.Compare(a.Value, b.Value)
		})
	}
	slices.SortFunc(g.Edges, func(a, b Edge) int {
		if c := strings.Compare(a.From, b.From); c != 0 {
			return c
		}
		if c := strings.Compare(a.To, b.To); c != 0 {
			return c
		}
		return strings.Compare(string(a.Relationship), string(b.Relationship))
	})
	for i := range g.Ambiguities {
		g.Ambiguities[i].At = g.Ambiguities[i].At.UTC()
		slices.Sort(g.Ambiguities[i].Candidates)
	}
	slices.SortFunc(g.Ambiguities, func(a, b Ambiguity) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(a.Label, b.Label)
	})
	for i := range g.Exclusions {
		g.Exclusions[i].At = g.Exclusions[i].At.UTC()
	}
	slices.SortFunc(g.Exclusions, func(a, b Exclusion) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(a.Label, b.Label)
	})
	slices.SortFunc(g.Coverage, func(a, b SourceCoverage) int { return strings.Compare(a.Source, b.Source) })
	g.computeGrades()
}

func compareObjectRef(a, b ObjectRef) int {
	for _, c := range [][2]string{{a.Kind, b.Kind}, {a.Namespace, b.Namespace}, {a.Name, b.Name}, {a.UID, b.UID}} {
		if r := strings.Compare(c[0], c[1]); r != 0 {
			return r
		}
	}
	return 0
}

// computeGrades assigns each node the strongest path grade reachable from the
// action node, where a path's grade is its weakest edge. Ownership is known in
// both directions, so STRUCTURAL_OWNER edges are also traversed from owned
// object to owner (for example from a deleted Pod to its ReplicaSet).
// Unreachable nodes keep an empty grade.
func (g *EffectGraph) computeGrades() {
	idx := make(map[string]int, len(g.Nodes))
	for i := range g.Nodes {
		idx[g.Nodes[i].ID] = i
		g.Nodes[i].Grade = ""
	}
	out := make(map[string][]Edge)
	for _, e := range g.Edges {
		out[e.From] = append(out[e.From], e)
		if e.Relationship == RelStructuralOwner {
			rev := e
			rev.From, rev.To = e.To, e.From
			out[e.To] = append(out[e.To], rev)
		}
	}
	root := ActionNodeID(g.Action.ID)
	ri, ok := idx[root]
	if !ok {
		return
	}
	best := map[string]PathGrade{root: GradeAttributed}
	g.Nodes[ri].Grade = GradeAttributed
	// Process grades strongest-first; each node settles at its best grade.
	for _, level := range []PathGrade{GradeAttributed, GradeCorrelated, GradeInferred} {
		var queue []string
		for id, gr := range best {
			if gr == level {
				queue = append(queue, id)
			}
		}
		slices.Sort(queue)
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, e := range out[cur] {
				ng := Weaker(level, GradeOf(e.Evidence))
				if prev, seen := best[e.To]; seen && gradeRank(prev) <= gradeRank(ng) {
					continue
				}
				best[e.To] = ng
				if ng == level {
					queue = append(queue, e.To)
				}
			}
		}
	}
	for id, gr := range best {
		g.Nodes[idx[id]].Grade = gr
	}
}

// MarshalCanonical returns the canonical JSON encoding of g. The receiver is
// canonicalized in place first.
func (g *EffectGraph) MarshalCanonical() ([]byte, error) {
	g.Canonicalize()
	if err := g.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	enc.SetIndent("", "  ")
	if err := enc.Encode(g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MaxGraphBytes bounds the size of a graph accepted by UnmarshalGraph.
const MaxGraphBytes = 16 << 20

// UnmarshalGraph decodes and validates a graph. Unknown fields are rejected so
// that a tampered or foreign document is not silently accepted.
func UnmarshalGraph(data []byte) (*EffectGraph, error) {
	if len(data) > MaxGraphBytes {
		return nil, fmt.Errorf("%w: graph exceeds %d bytes", ErrInvalidValue, MaxGraphBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var g EffectGraph
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidValue, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalidValue)
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	g.Canonicalize()
	return &g, nil
}
