// Package render turns effect graphs into terminal text, Graphviz DOT and
// Mermaid. All graph-provided strings are sanitized before output because an
// exported graph file may come from an untrusted source.
package render

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/effecttrace/effecttrace/pkg/model"
)

// Options control text rendering.
type Options struct {
	Color   bool
	Verbose bool
}

// clean removes control characters (including ANSI escapes) and bounds
// length.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		// Control characters and Unicode format characters (bidi
		// overrides, zero-width joiners) could rewrite or reorder what
		// the operator sees in a terminal.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (r >= 0x80 && r < 0xa0) {
			continue
		}
		b.WriteRune(r)
		if b.Len() > 400 {
			b.WriteString("…")
			break
		}
	}
	return b.String()
}

type palette struct{ on bool }

func (p palette) c(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// evidenceTag is the fixed-width label printed before each edge.
func evidenceTag(e model.EvidenceType) string {
	switch e {
	case model.EvidenceTemporalCorrelation:
		return "TEMPORAL CORRELATION"
	case model.EvidenceEventReference:
		return "EVENT REFERENCE"
	case model.EvidenceTraceLink:
		return "TRACE LINK"
	default:
		return string(e)
	}
}

func (p palette) evidence(e model.EvidenceType) string {
	tag := fmt.Sprintf("%-20s", evidenceTag(e))
	switch e {
	case model.EvidenceDirect:
		return p.c("1;32", tag)
	case model.EvidenceStructural:
		return p.c("36", tag)
	case model.EvidenceTraceLink:
		return p.c("34", tag)
	case model.EvidenceEventReference:
		return p.c("35", tag)
	case model.EvidenceTemporalCorrelation:
		return p.c("33", tag)
	default:
		return p.c("31", tag)
	}
}

func changeText(n model.Node, since time.Time) string {
	ch := n.Change
	if ch == nil {
		return ""
	}
	var parts []string
	switch ch.Type {
	case "MUTATED":
		if ch.GenerationFrom > 0 && ch.GenerationTo > 0 {
			parts = append(parts, fmt.Sprintf("generation %d -> %d", ch.GenerationFrom, ch.GenerationTo))
		} else {
			parts = append(parts, "mutated")
		}
		if ch.ReplicasFrom != nil && ch.ReplicasTo != nil {
			parts = append(parts, fmt.Sprintf("replicas %d -> %d", *ch.ReplicasFrom, *ch.ReplicasTo))
		}
	case "SCALED":
		if ch.ReplicasFrom != nil && ch.ReplicasTo != nil {
			parts = append(parts, fmt.Sprintf("scaled %d -> %d", *ch.ReplicasFrom, *ch.ReplicasTo))
		} else {
			parts = append(parts, "scaled")
		}
	case "CREATED":
		s := "created"
		if !ch.At.IsZero() && !since.IsZero() {
			s += " " + offset(ch.At.Sub(since))
		}
		if !ch.ReadyAt.IsZero() {
			s += ", ready " + offset(ch.ReadyAt.Sub(since))
		}
		if !ch.DeletedAt.IsZero() {
			s += ", deleted " + offset(ch.DeletedAt.Sub(since))
		}
		parts = append(parts, s)
	case "DELETED":
		s := "terminated"
		if !ch.At.IsZero() && !since.IsZero() {
			s += " " + offset(ch.At.Sub(since))
		}
		parts = append(parts, s)
	default:
		parts = append(parts, strings.ToLower(ch.Type))
	}
	return strings.Join(parts, ", ")
}

func offset(d time.Duration) string {
	sign := "+"
	if d < 0 {
		sign = "-"
	}
	a := d.Abs()
	if a < time.Second {
		return sign + a.Round(time.Millisecond).String()
	}
	return sign + a.Round(100*time.Millisecond).String()
}

// Explain writes a human-readable explanation of g.
func Explain(w io.Writer, g *model.EffectGraph, o Options) {
	p := palette{o.Color}
	a := g.Action
	nodes := map[string]model.Node{}
	for _, n := range g.Nodes {
		nodes[n.ID] = n
	}
	fmt.Fprintf(w, "%s  %s\n", p.c("1", "ACTION"), p.c("1", clean(a.Name)))
	meta := []string{string(a.Kind), "started " + a.StartedAt.UTC().Format("15:04:05.000Z07:00")}
	if a.Actor != "" {
		meta = append(meta, "actor "+clean(a.Actor))
	}
	if a.Trace != nil {
		meta = append(meta, "trace "+clean(a.Trace.TraceID))
	}
	meta = append(meta, "status "+string(g.Status))
	fmt.Fprintf(w, "        %s\n", strings.Join(meta, " · "))
	fmt.Fprintf(w, "        id %s\n\n", clean(a.ID))

	children := map[string][]model.Edge{}
	for _, e := range g.Edges {
		children[e.From] = append(children[e.From], e)
	}
	// Reverse ownership context edges (owner -> deleted pod) make the owner
	// reachable from the pod, mirroring how grades are computed.
	for _, e := range g.Edges {
		if e.Relationship == model.RelStructuralOwner && e.Rule == "controller-owner-of-directly-mutated-object" {
			if _, hasParent := hasIncomingNonOwner(g, e.From); !hasParent {
				rev := e
				rev.From, rev.To = e.To, e.From
				children[e.To] = append(children[e.To], rev)
			}
		}
	}
	for k := range children {
		slices.SortFunc(children[k], func(x, y model.Edge) int {
			nx, ny := nodes[x.To], nodes[y.To]
			if c := strings.Compare(kindOrder(nx), kindOrder(ny)); c != 0 {
				return c
			}
			if c := nx.ObservedAt.Compare(ny.ObservedAt); c != 0 {
				return c
			}
			return strings.Compare(nx.Label, ny.Label)
		})
	}
	seen := map[string]bool{model.ActionNodeID(a.ID): true}
	walk(w, p, o, g, nodes, children, []string{model.ActionNodeID(a.ID)}, 0, seen, a.StartedAt)

	if len(g.Ambiguities) > 0 {
		fmt.Fprintf(w, "\n%s\n", p.c("1;31", "NOT ATTRIBUTED (ambiguous)"))
		for _, am := range g.Ambiguities {
			fmt.Fprintf(w, "  %s  %s\n      candidates: %s\n      %s\n", offset(am.At.Sub(a.StartedAt)), clean(am.Label), clean(strings.Join(am.Candidates, ", ")), clean(am.Reason))
		}
	}
	if len(g.Exclusions) > 0 {
		fmt.Fprintf(w, "\n%s\n", p.c("1", "EXCLUDED (observed nearby, deliberately not attached)"))
		limit := len(g.Exclusions)
		if !o.Verbose && limit > 8 {
			limit = 8
		}
		for _, x := range g.Exclusions[:limit] {
			by := ""
			if x.ClaimedBy != "" {
				by = " [" + clean(x.ClaimedBy) + "]"
			}
			fmt.Fprintf(w, "  %s  %s: %s%s\n", offset(x.At.Sub(a.StartedAt)), clean(x.Label), clean(x.Reason), by)
		}
		if limit < len(g.Exclusions) {
			fmt.Fprintf(w, "  … %d more (use --verbose)\n", len(g.Exclusions)-limit)
		}
	}
	fmt.Fprintf(w, "\n%s\n", p.c("1", "WINDOWS"))
	for _, win := range g.Windows {
		state := ""
		if win.Open {
			state = " (open)"
		}
		fmt.Fprintf(w, "  %-34s %s .. %s%s\n      %s\n", clean(win.Name), offset(win.Start.Sub(a.StartedAt)), offset(win.End.Sub(a.StartedAt)), state, clean(win.Reason))
	}
	fmt.Fprintf(w, "\n%s\n", p.c("1", "COVERAGE"))
	for _, cv := range g.Coverage {
		mark := p.c("32", "yes")
		if !cv.Available {
			mark = p.c("31", "no ")
		}
		fmt.Fprintf(w, "  %-18s %s  %s\n", cv.Source, mark, clean(cv.Detail))
	}
	for _, n := range g.Notes {
		fmt.Fprintf(w, "\n%s %s\n", p.c("1;33", "IMPORTANT:"), clean(n))
	}
}

func hasIncomingNonOwner(g *model.EffectGraph, id string) (model.Edge, bool) {
	for _, e := range g.Edges {
		if e.To == id && e.Rule != "controller-owner-of-directly-mutated-object" {
			return e, true
		}
	}
	return model.Edge{}, false
}

func kindOrder(n model.Node) string {
	switch n.Type {
	case model.NodeKubernetesRequest:
		return "0"
	case model.NodeKubernetesObject:
		if n.Object != nil {
			switch n.Object.Kind {
			case "ReplicaSet", "StatefulSet":
				return "1"
			case "Pod":
				return "2"
			}
		}
		return "1"
	case model.NodeKubernetesEvent:
		return "3"
	case model.NodeMetricObservation:
		return "4"
	}
	return "5"
}

// walk prints the children of the given parents. Children of grouped
// siblings (for example the Events of three new Pods) are merged so that
// they are grouped too.
func walk(w io.Writer, p palette, o Options, g *model.EffectGraph, nodes map[string]model.Node, children map[string][]model.Edge, ids []string, depth int, seen map[string]bool, since time.Time) {
	var edges []model.Edge
	for _, id := range ids {
		edges = append(edges, children[id]...)
	}
	indent := strings.Repeat("  ", depth)
	// Group sibling Pods and Events with the same evidence and change type.
	type group struct {
		edge  model.Edge
		nodes []model.Node
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, e := range edges {
		n, ok := nodes[e.To]
		if !ok || seen[e.To] {
			continue
		}
		key := ""
		if !o.Verbose {
			switch {
			case n.Object != nil && n.Object.Kind == "Pod" && n.Change != nil:
				key = string(e.Evidence) + "/pod/" + n.Change.Type
			case n.Type == model.NodeKubernetesEvent && n.Event != nil:
				key = string(e.Evidence) + "/event/" + n.Event.Reason
			}
		}
		if key != "" {
			if gr, ok := byKey[key]; ok {
				gr.nodes = append(gr.nodes, n)
				seen[e.To] = true
				continue
			}
		}
		gr := &group{edge: e, nodes: []model.Node{n}}
		groups = append(groups, gr)
		if key != "" {
			byKey[key] = gr
		}
		seen[e.To] = true
	}
	for _, gr := range groups {
		e := gr.edge
		n := gr.nodes[0]
		label := clean(n.Label)
		detail := changeText(n, since)
		if n.Metric != nil && n.Metric.Changed {
			detail = ""
		}
		if len(gr.nodes) > 1 {
			switch n.Type {
			case model.NodeKubernetesEvent:
				label = fmt.Sprintf("%d × Event %s", len(gr.nodes), clean(n.Label))
			default:
				ready := 0
				for _, x := range gr.nodes {
					if x.Change != nil && !x.Change.ReadyAt.IsZero() {
						ready++
					}
				}
				verb := "created"
				if n.Change.Type == "DELETED" {
					verb = "terminated"
				}
				label = fmt.Sprintf("%d Pods %s", len(gr.nodes), verb)
				detail = ""
				if verb == "created" {
					detail = fmt.Sprintf("%d/%d ready", ready, len(gr.nodes))
				}
			}
		} else if n.Type == model.NodeKubernetesEvent && n.Event != nil {
			label = "Event " + clean(n.Event.Reason)
			if n.Event.Note != "" {
				detail = clean(n.Event.Note)
			}
		}
		line := fmt.Sprintf("%s%s %s", indent, p.evidence(e.Evidence), label)
		if detail != "" {
			line += p.c("2", "  "+detail)
		}
		if e.Evidence == model.EvidenceTemporalCorrelation && n.Metric != nil {
			line += "\n" + indent + strings.Repeat(" ", 21) + p.c("33", clean(metricLine(n.Metric)))
		}
		fmt.Fprintln(w, line)
		if o.Verbose {
			fmt.Fprintf(w, "%s%s %s\n", indent, strings.Repeat(" ", 20), p.c("2", "why: "+clean(e.Reason)))
		}
		var next []string
		for _, x := range gr.nodes {
			next = append(next, x.ID)
		}
		walk(w, p, o, g, nodes, children, next, depth+1, seen, since)
	}
}

func metricLine(m *model.MetricInfo) string {
	return fmt.Sprintf("%s %s during the observation window (baseline %s, observed %s)", m.Signal, m.Direction, fmtVal(m.Baseline, m.Unit), fmtVal(m.Observed, m.Unit))
}

func fmtVal(v float64, unit string) string {
	switch unit {
	case "ratio":
		return fmt.Sprintf("%.2f%%", v*100)
	case "seconds":
		return fmt.Sprintf("%.0fms", v*1000)
	default:
		return fmt.Sprintf("%.3g", v)
	}
}

// DOT renders g as a Graphviz digraph. Correlation edges are dashed.
func DOT(w io.Writer, g *model.EffectGraph) {
	q := func(s string) string {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(clean(s), `\`, `\\`), `"`, `\"`) + `"`
	}
	fmt.Fprintf(w, "digraph effecttrace {\n  rankdir=LR;\n  node [shape=box, fontname=\"Helvetica\"];\n")
	for _, n := range g.Nodes {
		shape := "box"
		switch n.Type {
		case model.NodeAction:
			shape = "doubleoctagon"
		case model.NodeKubernetesEvent:
			shape = "note"
		case model.NodeMetricObservation:
			shape = "ellipse"
		}
		fmt.Fprintf(w, "  %s [label=%s, shape=%s];\n", q(n.ID), q(n.Label), shape)
	}
	for _, e := range g.Edges {
		style := "solid"
		if e.Evidence == model.EvidenceTemporalCorrelation || e.Evidence == model.EvidenceInferred {
			style = "dashed"
		}
		fmt.Fprintf(w, "  %s -> %s [label=%s, style=%s];\n", q(e.From), q(e.To), q(string(e.Evidence)), style)
	}
	fmt.Fprintln(w, "}")
}

// Mermaid renders g as a Mermaid flowchart. Correlation edges are dotted.
func Mermaid(w io.Writer, g *model.EffectGraph) {
	ids := map[string]string{}
	for i, n := range g.Nodes {
		ids[n.ID] = fmt.Sprintf("n%d", i)
	}
	esc := func(s string) string {
		s = clean(s)
		for _, r := range []string{`"`, "<", ">", "[", "]", "{", "}", "|"} {
			s = strings.ReplaceAll(s, r, " ")
		}
		return s
	}
	fmt.Fprintln(w, "flowchart LR")
	for _, n := range g.Nodes {
		fmt.Fprintf(w, "  %s[\"%s\"]\n", ids[n.ID], esc(n.Label))
	}
	for _, e := range g.Edges {
		arrow := "-->"
		if e.Evidence == model.EvidenceTemporalCorrelation || e.Evidence == model.EvidenceInferred {
			arrow = "-.->"
		}
		fmt.Fprintf(w, "  %s %s|%s| %s\n", ids[e.From], arrow, esc(string(e.Evidence)), ids[e.To])
	}
}
