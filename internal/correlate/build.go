package correlate

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/pkg/model"
)

// Attribution rule names recorded on edges. They are documented in
// docs/evidence-model.md.
const (
	RuleClientSpanDescendant = "client-span-descends-from-tool-span"
	RuleRequestDuringTool    = "request-received-during-tool-span"
	RuleResponseUID          = "uid-from-client-response"
	RuleAuditUID             = "uid-from-audit-object-ref"
	RuleNameAtRequestTime    = "uid-resolved-by-name-at-request-time"
	RuleUnresolved           = "target-not-observed"
	RuleOwnerInWindow        = "controller-owned-change-in-window-single-claimant"
	RuleReplacementInWindow  = "replacement-of-deleted-pod-in-window-single-claimant"
	RuleOwnerContext         = "controller-owner-of-directly-mutated-object"
	RuleEventInWindow        = "event-references-object-in-window-single-claimant"
	RuleMetricInWindow       = "signal-changed-in-observation-window"
)

// change is a lifecycle change of an object inside a window.
type change struct {
	typ      string // CREATED, DELETED, SCALED
	at       time.Time
	from, to *int32
	readyAt  time.Time
}

type builder struct {
	v     store.View
	ix    *index
	a     *action
	cfg   Config
	opts  Options
	g     *model.EffectGraph
	nodes map[string]*model.Node
	edges map[string]model.Edge
	// attached records object UIDs placed in the graph as effects.
	attached map[string]bool
	ambig    map[string]bool
	// self holds the action IDs whose claims count as this graph's own: the
	// action itself and, for temporally linked requests, the request's own
	// API-call action.
	self map[string]bool
}

func build(v store.View, ix *index, a *action, cfg Config, opts Options) *model.EffectGraph {
	b := &builder{
		v: v, ix: ix, a: a, cfg: cfg, opts: opts,
		g: &model.EffectGraph{
			SchemaVersion: model.SchemaVersion,
			ID:            "g-" + a.ID,
			Action:        a.Action,
		},
		nodes:    map[string]*model.Node{},
		edges:    map[string]model.Edge{},
		attached: map[string]bool{},
		ambig:    map[string]bool{},
		self:     map[string]bool{a.ID: true},
	}
	for _, r := range a.requests {
		if r.link == linkTemporal && r.audit != nil {
			b.self["k8s-"+r.audit.AuditID] = true
		}
	}
	b.g.Action.Targets = slices.Clone(a.Targets)
	b.addActionAndRequests()
	b.addStructuralEffects()
	b.addEvents()
	b.addMetrics()
	b.addExclusions()
	b.finish()
	return b.g
}

func (b *builder) addNode(n model.Node) *model.Node {
	if cur, ok := b.nodes[n.ID]; ok {
		if cur.Change == nil && n.Change != nil {
			cur.Change = n.Change
		}
		return cur
	}
	c := n
	b.nodes[n.ID] = &c
	return &c
}

func (b *builder) addEdge(from, to string, rel model.Relationship, reason, source, rule string, at time.Time, facts ...model.Fact) {
	e, err := model.NewEdge(from, to, rel, reason, source, at)
	if err != nil {
		return
	}
	e.Rule = rule
	e.Facts = facts
	if _, ok := b.edges[e.ID]; !ok {
		b.edges[e.ID] = e
	}
}

func objNodeID(uid string, ref model.ObjectRef, how resolution) string {
	if uid != "" {
		return "k8s:" + uid
	}
	suffix := ""
	if how == resolvedAmbig {
		suffix = ":ambiguous"
	}
	return "k8s:unresolved:" + ref.Kind + "/" + ref.Namespace + "/" + ref.Name + suffix
}

func describe(ref model.ObjectRef) string {
	if ref.Namespace != "" {
		return ref.Kind + " " + ref.Namespace + "/" + ref.Name
	}
	return ref.Kind + " " + ref.Name
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func requestInfo(r *request) *model.RequestInfo {
	ri := &model.RequestInfo{
		Verb: r.verb, Resource: r.resource, Subresource: r.subres, APIGroup: r.group,
		Namespace: r.ns, Name: r.name, ReceivedAt: r.at, StatusCode: r.statusCode(),
	}
	if r.span != nil {
		ri.Sources = append(ri.Sources, "otlp-client-span")
	}
	if r.audit != nil {
		ri.AuditID = r.audit.AuditID
		ri.User = r.audit.User
		ri.UserAgent = r.audit.UserAgent
		ri.Sources = append(ri.Sources, "kubernetes-audit")
	}
	if ri.Sources == nil {
		ri.Sources = []string{}
	}
	return ri
}

func requestLabel(r *request) string {
	target := r.resource
	if r.subres != "" {
		target += "/" + r.subres
	}
	obj := r.name
	if r.ns != "" {
		obj = r.ns + "/" + r.name
	}
	return strings.TrimSpace(strings.ToUpper(r.verb) + " " + target + " " + obj)
}

func (b *builder) addActionAndRequests() {
	a := b.a
	root := model.ActionNodeID(a.ID)
	an := model.Node{ID: root, Type: model.NodeAction, Label: a.Name, Span: a.Trace, ObservedAt: a.StartedAt}
	if a.Kind == model.ActionKubernetesAPICall && len(a.requests) == 1 {
		an.Request = requestInfo(a.requests[0])
	}
	b.addNode(an)

	muts := b.ix.byAction[a.ID]
	for _, r := range a.requests {
		origin := root
		switch r.link {
		case linkTrace:
			b.addNode(model.Node{ID: r.nodeID, Type: model.NodeKubernetesRequest, Label: requestLabel(r),
				Request: requestInfo(r), Span: &model.TraceRef{TraceID: r.span.TraceID, SpanID: r.span.SpanID}, ObservedAt: r.at})
			facts := []model.Fact{
				{Key: "trace_id", Value: r.span.TraceID},
				{Key: "client_span_id", Value: r.span.SpanID},
				{Key: "parent_hops", Value: fmt.Sprint(r.traceDepth)},
			}
			b.addEdge(root, r.nodeID, model.RelTraceParent,
				fmt.Sprintf("The Kubernetes client span %q descends from the tool span (%d parent hop(s)) in trace %s.", r.span.Name, r.traceDepth, shortID(r.span.TraceID)),
				"otlp", RuleClientSpanDescendant, r.at, facts...)
			origin = r.nodeID
			if r.auditMismatch {
				b.g.Notes = append(b.g.Notes, "A client span named an audit ID whose audit event disagrees on verb or object; the audit event was not used as confirmation.")
			}
		case linkTemporal:
			b.addNode(model.Node{ID: r.nodeID, Type: model.NodeKubernetesRequest, Label: requestLabel(r), Request: requestInfo(r), ObservedAt: r.at})
			b.addEdge(root, r.nodeID, model.RelTemporalCorrelation,
				"The request was received while the tool span was running, but no trace context or audit ID links it to the tool call. Correlation only; it may have been made by another actor.",
				"kubernetes-audit", RuleRequestDuringTool, r.at,
				model.Fact{Key: "audit_id", Value: r.audit.AuditID}, model.Fact{Key: "user", Value: r.audit.User})
			origin = r.nodeID
		}
		if !successful(r.statusCode()) {
			continue
		}
		var m *mutation
		for _, cand := range muts {
			if cand.req == r {
				m = cand
				break
			}
		}
		if m == nil {
			continue
		}
		b.addMutation(origin, m)
	}
}

func (b *builder) addMutation(origin string, m *mutation) {
	r := m.req
	ref := model.ObjectRef{Kind: m.kind, Namespace: r.ns, Name: r.name, UID: m.uid}
	if h, ok := b.v.Object(m.uid); ok {
		ref = refOf(h)
	}
	ch := &model.ObjectChange{Type: m.change, GenerationFrom: m.genFrom, GenerationTo: m.genTo,
		ResourceVersion: m.rv, ReplicasFrom: m.replFrom, ReplicasTo: m.replTo, At: m.observedAt}
	if ch.GenerationFrom == ch.GenerationTo {
		ch.GenerationFrom = 0
	}
	id := objNodeID(m.uid, ref, m.how)
	b.addNode(model.Node{ID: id, Type: model.NodeKubernetesObject, Label: describe(ref), Object: &ref, Change: ch, ObservedAt: m.t0})
	if m.uid != "" {
		b.attached[m.uid] = true
	}

	var facts []model.Fact
	var parts []string
	verbObj := requestLabel(r)
	parts = append(parts, verbObj+" changed "+describe(ref))
	if m.uid != "" {
		facts = append(facts, model.Fact{Key: "object_uid", Value: m.uid})
	}
	if ch.GenerationFrom > 0 && ch.GenerationTo > 0 {
		parts[0] += fmt.Sprintf(" (generation %d -> %d)", ch.GenerationFrom, ch.GenerationTo)
	}
	rule := ""
	switch m.how {
	case resolvedResponse:
		rule = RuleResponseUID
		parts = append(parts, "the UID and resourceVersion were returned to the instrumented client")
		facts = append(facts, model.Fact{Key: "uid_source", Value: "client response metadata"})
	case resolvedAudit:
		rule = RuleAuditUID
		parts = append(parts, "the UID is recorded in the audit event objectRef")
		facts = append(facts, model.Fact{Key: "uid_source", Value: "audit objectRef"})
	case resolvedName:
		rule = RuleNameAtRequestTime
		parts = append(parts, "the UID was resolved by name: it was the only "+ref.Kind+" with that name at request time")
		facts = append(facts, model.Fact{Key: "uid_source", Value: "name at request time"})
	case resolvedAmbig:
		rule = RuleUnresolved
		parts = append(parts, "more than one object carried that name around the request time, so no UID is asserted")
	default:
		rule = RuleUnresolved
		parts = append(parts, "the object was not observed by the watch, so it is identified by name only")
	}
	if r.audit != nil {
		facts = append(facts, model.Fact{Key: "audit_id", Value: r.audit.AuditID})
		if r.span != nil {
			parts = append(parts, "audit event "+shortID(r.audit.AuditID)+" independently confirms the request")
			facts = append(facts, model.Fact{Key: "audit_id_match", Value: "client span Audit-ID equals audit event auditID"})
		}
	}
	if m.rv != "" && !m.observedAt.IsZero() {
		facts = append(facts, model.Fact{Key: "resource_version", Value: m.rv})
		if r.respRV != "" && r.respRV == m.rv {
			parts = append(parts, "the watch observed the same resourceVersion")
		}
	}
	if !m.changed && m.uid != "" {
		parts = append(parts, "no resulting spec change, creation or deletion was observed, so no controller fan-out is attributed")
		ch.Type = "UNCHANGED"
	}
	src := "kubernetes-audit"
	if r.span != nil {
		src = "otlp"
	}
	b.addEdge(origin, id, model.RelDirectRequest, strings.Join(parts, "; ")+".", src, rule, m.t0, facts...)
	b.g.Windows = append(b.g.Windows, model.ObservationWindow{
		Name: "reconcile " + describe(ref), Start: m.win.start, End: m.win.end, Reason: m.win.reason, Open: m.win.open,
	})
}

// claimants returns the actions whose scope and window cover a change of
// object uid at time at. direct is true when the change is itself the direct
// target of a request (direct claims take precedence over structural ones).
func (b *builder) claimants(uid, typ string, at time.Time) (ids []string, direct bool) {
	set := map[string]bool{}
	for _, m := range b.ix.byUID[uid] {
		if m.change == typ && !at.Before(m.t0.Add(-b.cfg.SkewTolerance)) && !at.After(m.win.end) {
			set[m.act.ID] = true
		}
	}
	if len(set) > 0 {
		return sortedKeys(set), true
	}
	for _, anc := range ancestors(b.v, uid, b.cfg.MaxDepth) {
		for _, m := range b.ix.byUID[anc] {
			if m.win.contains(at) {
				set[m.act.ID] = true
			}
		}
	}
	if typ == "CREATED" {
		if h, ok := b.v.Object(uid); ok {
			if o, ok := h.ControllerOwner(); ok {
				for _, m := range b.ix.delByOwner[o.UID] {
					if m.uid != uid && m.win.contains(at) {
						set[m.act.ID] = true
					}
				}
			}
		}
	}
	return sortedKeys(set), false
}

// decide classifies claimants: attach when every claimant is this graph's
// own action, ambiguous when own and foreign claimants compete.
func (b *builder) decide(ids []string) (attach, ambiguous bool) {
	own, foreign := 0, 0
	for _, id := range ids {
		if b.self[id] {
			own++
		} else {
			foreign++
		}
	}
	return own > 0 && foreign == 0, own > 0 && foreign > 0
}

func (b *builder) ownsAny(ids []string) bool {
	for _, id := range ids {
		if b.self[id] {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// changesIn returns lifecycle changes of h inside w.
func changesIn(h *store.ObjectHistory, w window) []change {
	var out []change
	created := h.CreatedAt()
	deleted := h.DeletedAt()
	if w.contains(created) && !h.First().Initial {
		c := change{typ: "CREATED", at: created}
		for _, o := range h.Observations {
			if o.Ready != nil && *o.Ready {
				c.readyAt = o.At
				break
			}
		}
		out = append(out, c)
	}
	// A deletion is a separate change with its own claimants: an object
	// created by one action may be deleted by another action's rollout.
	if !deleted.IsZero() && w.contains(deleted) {
		out = append(out, change{typ: "DELETED", at: deleted})
	}
	if h.Ref.Kind == "ReplicaSet" || h.Ref.Kind == "StatefulSet" {
		var first, last *obs.ObjectObservation
		var at time.Time
		for i := 1; i < len(h.Observations); i++ {
			prev, cur := h.Observations[i-1], h.Observations[i]
			if prev.Replicas == nil || cur.Replicas == nil || *prev.Replicas == *cur.Replicas || !w.contains(cur.At) {
				continue
			}
			if first == nil {
				first = &h.Observations[i-1]
				at = cur.At
			}
			last = &h.Observations[i]
		}
		if first != nil && (len(out) == 0 || out[0].typ != "CREATED") {
			out = append(out, change{typ: "SCALED", at: at, from: first.Replicas, to: last.Replicas})
		}
	}
	return out
}

func changeVerb(c change) string {
	switch c.typ {
	case "CREATED":
		return "created"
	case "DELETED":
		return "terminated"
	case "SCALED":
		if c.from != nil && c.to != nil {
			return fmt.Sprintf("scaled %d -> %d", *c.from, *c.to)
		}
		return "scaled"
	}
	return strings.ToLower(c.typ)
}

func (b *builder) addStructuralEffects() {
	for _, m := range b.ix.byAction[b.a.ID] {
		if m.uid == "" || !m.changed {
			continue
		}
		if m.kind == "Pod" && m.change == "DELETED" && m.owner != "" {
			b.addReplacementEffects(m)
			continue
		}
		b.walkDescendants(m, objNodeID(m.uid, model.ObjectRef{}, m.how), m.uid, 1)
	}
}

func (b *builder) walkDescendants(m *mutation, parentNode, parentUID string, depth int) {
	if depth > b.cfg.MaxDepth {
		return
	}
	parentHist, _ := b.v.Object(parentUID)
	for _, child := range b.v.Children(parentUID) {
		h, ok := b.v.Object(child)
		if !ok {
			continue
		}
		ref := refOf(h)
		childNode := "k8s:" + child
		attachedHere := false
		for _, c := range changesIn(h, m.win) {
			ids, direct := b.claimants(child, c.typ, c.at)
			label := describe(ref) + " " + changeVerb(c)
			attach, ambiguous := b.decide(ids)
			switch {
			case direct && !b.ownsAny(ids):
				b.exclude(label, &ref, c.typ, c.at, "directly changed by another action", strings.Join(ids, ","))
			case attach:
				b.attachChange(m, parentNode, parentHist, childNode, ref, c, RuleOwnerInWindow)
				attachedHere = true
			case ambiguous:
				b.ambiguity(label, &ref, c.at, ids, "the change falls in the reconciliation windows of more than one action that changed an owner of this object")
			}
		}
		next := parentNode
		if attachedHere || b.nodes[childNode] != nil {
			next = childNode
		} else if b.hasChangedDescendant(child, m.win, depth+1) {
			// Keep an unchanged intermediate owner as context so that the
			// ownership path to changed descendants stays explicit.
			b.addNode(model.Node{ID: childNode, Type: model.NodeKubernetesObject, Label: describe(ref), Object: &ref, ObservedAt: h.First().At})
			b.addEdge(parentNode, childNode, model.RelStructuralOwner,
				fmt.Sprintf("%s is controlled by %s (ownerReference uid %s); it did not change itself but owns objects that did.", describe(ref), labelOf(parentHist), shortID(parentUID)),
				"kubernetes-watch", RuleOwnerContext, h.First().At, model.Fact{Key: "owner_uid", Value: parentUID})
			next = childNode
		}
		if next == childNode {
			b.walkDescendants(m, childNode, child, depth+1)
		}
	}
}

func (b *builder) hasChangedDescendant(uid string, w window, depth int) bool {
	if depth > b.cfg.MaxDepth {
		return false
	}
	for _, c := range b.v.Children(uid) {
		if h, ok := b.v.Object(c); ok && len(changesIn(h, w)) > 0 {
			return true
		}
		if b.hasChangedDescendant(c, w, depth+1) {
			return true
		}
	}
	return false
}

func labelOf(h *store.ObjectHistory) string {
	if h == nil {
		return "its owner"
	}
	return describe(h.Ref)
}

func (b *builder) attachChange(m *mutation, parentNode string, parentHist *store.ObjectHistory, childNode string, ref model.ObjectRef, c change, rule string) {
	if cur, ok := b.nodes[childNode]; ok && cur.Change != nil && cur.Change.Type == "CREATED" && c.typ == "DELETED" {
		// The same action created and later deleted the object.
		cur.Change.DeletedAt = c.at
		return
	}
	ch := &model.ObjectChange{Type: c.typ, At: c.at, ReplicasFrom: c.from, ReplicasTo: c.to, ReadyAt: c.readyAt}
	if h, ok := b.v.Object(ref.UID); ok {
		ch.Revision = h.Last().Revision
	}
	b.addNode(model.Node{ID: childNode, Type: model.NodeKubernetesObject, Label: describe(ref), Object: &ref, Change: ch, ObservedAt: c.at})
	b.attached[ref.UID] = true
	delta := roundDelta(c.at.Sub(m.t0))
	reason := fmt.Sprintf("%s is controlled by %s (ownerReference uid %s) and was %s %s after the request, inside the reconciliation window; no other action's window covers it.",
		describe(ref), labelOf(parentHist), shortID(parentHistUID(parentHist)), changeVerb(c), delta)
	if rule == RuleReplacementInWindow {
		reason = fmt.Sprintf("%s is controlled by %s, which owned the deleted Pod; it was created %s after the deletion request, inside the window; no other action's window covers it.",
			describe(ref), labelOf(parentHist), delta)
	}
	b.addEdge(parentNode, childNode, model.RelStructuralOwner, reason, "kubernetes-watch", rule, c.at,
		model.Fact{Key: "owner_uid", Value: parentHistUID(parentHist)},
		model.Fact{Key: "change", Value: c.typ})
}

func parentHistUID(h *store.ObjectHistory) string {
	if h == nil {
		return ""
	}
	return h.Ref.UID
}

func (b *builder) addReplacementEffects(m *mutation) {
	oh, ok := b.v.Object(m.owner)
	if !ok {
		return
	}
	ownerRef := refOf(oh)
	ownerNode := "k8s:" + m.owner
	b.addNode(model.Node{ID: ownerNode, Type: model.NodeKubernetesObject, Label: describe(ownerRef), Object: &ownerRef, ObservedAt: oh.First().At})
	podNode := objNodeID(m.uid, model.ObjectRef{}, m.how)
	b.addEdge(ownerNode, podNode, model.RelStructuralOwner,
		fmt.Sprintf("%s is the controller owner of the deleted Pod (ownerReference uid %s).", describe(ownerRef), shortID(m.owner)),
		"kubernetes-watch", RuleOwnerContext, m.t0, model.Fact{Key: "owner_uid", Value: m.owner})
	for _, child := range b.v.Children(m.owner) {
		if child == m.uid {
			continue
		}
		h, ok := b.v.Object(child)
		if !ok {
			continue
		}
		ref := refOf(h)
		for _, c := range changesIn(h, m.win) {
			if c.typ != "CREATED" {
				continue
			}
			ids, direct := b.claimants(child, c.typ, c.at)
			label := describe(ref) + " " + changeVerb(c)
			attach, ambiguous := b.decide(ids)
			switch {
			case direct && !b.ownsAny(ids):
				b.exclude(label, &ref, c.typ, c.at, "directly changed by another action", strings.Join(ids, ","))
			case attach:
				b.attachChange(m, ownerNode, oh, "k8s:"+child, ref, c, RuleReplacementInWindow)
			case ambiguous:
				b.ambiguity(label, &ref, c.at, ids, "the replacement falls in the windows of more than one action affecting this owner")
			}
		}
	}
}

func (b *builder) ambiguity(label string, ref *model.ObjectRef, at time.Time, ids []string, reason string) {
	key := label + at.String()
	if b.ambig[key] {
		return
	}
	b.ambig[key] = true
	b.g.Ambiguities = append(b.g.Ambiguities, model.Ambiguity{Label: label, Object: ref, At: at, Candidates: ids, Reason: reason})
}

func (b *builder) exclude(label string, ref *model.ObjectRef, kind string, at time.Time, reason, claimedBy string) {
	if len(b.g.Exclusions) >= b.cfg.MaxExclusions {
		return
	}
	for _, e := range b.g.Exclusions {
		if e.Label == label && e.At.Equal(at) {
			return
		}
	}
	b.g.Exclusions = append(b.g.Exclusions, model.Exclusion{Label: label, Object: ref, Kind: kind, At: at, Reason: reason, ClaimedBy: claimedBy})
}

func (b *builder) addEvents() {
	muts := b.ix.byAction[b.a.ID]
	if len(muts) == 0 {
		return
	}
	var ids []string
	for id, n := range b.nodes {
		if n.Object != nil && n.Object.UID != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, nid := range ids {
		n := b.nodes[nid]
		uid := n.Object.UID
		for _, ev := range b.v.EventsRegarding(uid) {
			covered := false
			for _, m := range muts {
				if m.win.contains(ev.At) {
					covered = true
					break
				}
			}
			if !covered {
				continue
			}
			claims := map[string]bool{}
			for _, m := range b.ix.byUID[uid] {
				if m.win.contains(ev.At) {
					claims[m.act.ID] = true
				}
			}
			for _, anc := range ancestors(b.v, uid, b.cfg.MaxDepth) {
				for _, m := range b.ix.byUID[anc] {
					if m.win.contains(ev.At) {
						claims[m.act.ID] = true
					}
				}
			}
			if o, ok := b.ownerOfReplacement(uid); ok {
				for _, m := range b.ix.delByOwner[o] {
					if m.win.contains(ev.At) {
						claims[m.act.ID] = true
					}
				}
			}
			if n.Object.Kind == "ReplicaSet" {
				// ReplicaSet events about Pod creation in a delete-replacement.
				for _, m := range b.ix.delByOwner[uid] {
					if m.win.contains(ev.At) {
						claims[m.act.ID] = true
					}
				}
			}
			ref := n.Object
			label := fmt.Sprintf("Event %s on %s", ev.Reason, describe(*ref))
			list := sortedKeys(claims)
			attach, ambiguous := b.decide(list)
			if ambiguous {
				b.ambiguity(label, ref, ev.At, list, "the Event falls in the windows of more than one action covering this object")
				continue
			}
			if !attach {
				continue
			}
			eid := "event:" + ev.UID
			b.addNode(model.Node{ID: eid, Type: model.NodeKubernetesEvent, Label: ev.Reason,
				Event: &model.EventInfo{Reason: ev.Reason, Type: ev.Type, Note: ev.Note, Controller: ev.Controller, Count: ev.Count}, ObservedAt: ev.At})
			b.addEdge(nid, eid, model.RelEventReference,
				fmt.Sprintf("Kubernetes Event %s references %s by UID %s.", ev.Reason, describe(*ref), shortID(uid)),
				"kubernetes-events", RuleEventInWindow, ev.At, model.Fact{Key: "regarding_uid", Value: uid}, model.Fact{Key: "event_uid", Value: ev.UID})
		}
	}
}

func (b *builder) ownerOfReplacement(uid string) (string, bool) {
	h, ok := b.v.Object(uid)
	if !ok || h.Ref.Kind != "Pod" {
		return "", false
	}
	o, ok := h.ControllerOwner()
	return o.UID, ok
}

func (b *builder) addMetrics() {
	for _, r := range b.v.Metrics(b.a.ID) {
		nodeID := "k8s:" + r.WorkloadUID
		_, inGraph := b.nodes[nodeID]
		if r.Error != "" {
			continue
		}
		label := fmt.Sprintf("%s for %s", r.Signal, r.Workload)
		if !inGraph {
			if r.Changed {
				ref := &model.ObjectRef{Kind: "Workload", Namespace: r.Namespace, Name: r.Workload, UID: r.WorkloadUID}
				b.exclude(fmt.Sprintf("%s %s from %s to %s", label, r.Direction, fmtVal(r.Baseline, r.Unit), fmtVal(r.Observed, r.Unit)), ref, "METRIC", r.WindowStart,
					"the signal changed during the window, but "+r.Workload+" is outside this action's structural scope", "")
			}
			continue
		}
		mid := "metric:" + r.Signal + ":" + r.WorkloadUID
		b.addNode(model.Node{ID: mid, Type: model.NodeMetricObservation, Label: label, ObservedAt: r.WindowStart,
			Metric: &model.MetricInfo{Signal: r.Signal, Unit: r.Unit, Workload: r.Workload, Baseline: r.Baseline,
				Observed: r.Observed, Direction: r.Direction, Changed: r.Changed, Samples: r.Samples}})
		if !r.Changed {
			continue
		}
		b.addEdge(nodeID, mid, model.RelTemporalCorrelation,
			fmt.Sprintf("%s %s from %s (baseline) to %s during the observation window %s - %s. Temporal correlation does not prove causation.",
				label, r.Direction, fmtVal(r.Baseline, r.Unit), fmtVal(r.Observed, r.Unit), r.WindowStart.UTC().Format(time.TimeOnly), r.WindowEnd.UTC().Format(time.TimeOnly)),
			"prometheus", RuleMetricInWindow, r.WindowStart,
			model.Fact{Key: "baseline", Value: fmtVal(r.Baseline, r.Unit)}, model.Fact{Key: "observed", Value: fmtVal(r.Observed, r.Unit)},
			model.Fact{Key: "samples", Value: fmt.Sprint(r.Samples)})
		if !slices.ContainsFunc(b.g.Windows, func(w model.ObservationWindow) bool { return w.Name == "telemetry" }) {
			b.g.Windows = append(b.g.Windows,
				model.ObservationWindow{Name: "baseline", Start: r.BaselineStart, End: r.WindowStart, Reason: "telemetry comparison baseline before the action"},
				model.ObservationWindow{Name: "telemetry", Start: r.WindowStart, End: r.WindowEnd, Reason: "reconciliation window plus telemetry settle period"})
		}
	}
}

func fmtVal(v float64, unit string) string {
	switch unit {
	case "ratio":
		return fmt.Sprintf("%.2f%%", v*100)
	case "seconds":
		return fmt.Sprintf("%.0fms", v*1000)
	case "":
		return fmt.Sprintf("%.3g", v)
	default:
		return fmt.Sprintf("%.3g %s", v, unit)
	}
}

func (b *builder) addExclusions() {
	muts := b.ix.byAction[b.a.ID]
	var lo, hi time.Time
	nsSet := map[string]bool{}
	for _, m := range muts {
		if m.req.ns != "" {
			nsSet[m.req.ns] = true
		}
		if lo.IsZero() || m.win.start.Before(lo) {
			lo = m.win.start
		}
		if m.win.end.After(hi) {
			hi = m.win.end
		}
	}
	w := window{start: lo, end: hi}
	for _, ns := range sortedKeys(nsSet) {
		for _, h := range b.v.ObjectsInNamespace(ns) {
			if b.attached[h.Ref.UID] {
				continue
			}
			ref := refOf(h)
			for _, c := range changesIn(h, w) {
				ids, _ := b.claimants(h.Ref.UID, c.typ, c.at)
				label := describe(ref) + " " + changeVerb(c)
				if b.ownsAny(ids) {
					continue // reported as attached or ambiguous already
				}
				if len(ids) > 0 {
					b.exclude(label, &ref, c.typ, c.at, "attributed to another action", strings.Join(ids, ","))
				} else {
					b.exclude(label, &ref, c.typ, c.at, "no ownership path to an object this action changed", "")
				}
			}
		}
	}
}

func (b *builder) finish() {
	g := b.g
	for _, n := range b.nodes {
		g.Nodes = append(g.Nodes, *n)
	}
	for _, e := range b.edges {
		g.Edges = append(g.Edges, e)
	}
	open := false
	for _, w := range g.Windows {
		open = open || w.Open
	}
	muts := b.ix.byAction[b.a.ID]
	metrics := b.v.Metrics(b.a.ID)
	switch {
	case open:
		g.Status = model.StatusObserving
	case b.opts.TelemetryConfigured && len(metrics) == 0 && len(muts) > 0:
		g.Status = model.StatusSettling
	default:
		g.Status = model.StatusComplete
	}
	g.Coverage = b.coverage(muts, metrics)
	hasTemporal := false
	for _, e := range g.Edges {
		if e.Evidence == model.EvidenceTemporalCorrelation {
			hasTemporal = true
		}
	}
	if hasTemporal {
		g.Notes = append(g.Notes, "Temporal correlation does not prove causation.")
	}
	if len(g.Ambiguities) > 0 {
		g.Notes = append(g.Notes, "Some changes were not attributed because more than one action's window covered them; see ambiguities.")
	}
	if len(muts) == 0 && b.a.Kind == model.ActionMCPToolCall {
		g.Notes = append(g.Notes, "No Kubernetes request is linked to this tool call by trace context or audit ID.")
	}
	slices.Sort(g.Notes)
	g.Notes = slices.Compact(g.Notes)
	g.Canonicalize()
}

func (b *builder) coverage(muts []*mutation, metrics []obs.MetricResult) []model.SourceCoverage {
	var out []model.SourceCoverage
	a := b.a
	// Audit confirmation of the action's requests.
	total, confirmed := 0, 0
	for _, r := range a.requests {
		total++
		if r.audit != nil {
			confirmed++
		}
	}
	auditDetail := fmt.Sprintf("%d/%d request(s) confirmed by an audit event", confirmed, total)
	if st, ok := b.v.Source("kubernetes-audit"); ok && !st.Healthy {
		auditDetail += "; source unhealthy: " + st.Detail
	} else if !ok {
		auditDetail += "; audit source not configured"
	}
	out = append(out, model.SourceCoverage{Source: "kubernetes-audit", Available: total > 0 && confirmed == total, Detail: auditDetail})

	if a.Kind == model.ActionMCPToolCall {
		out = append(out, model.SourceCoverage{Source: "otlp", Available: true, Detail: "tool span received"})
	} else {
		st, ok := b.v.Source("otlp")
		out = append(out, model.SourceCoverage{Source: "otlp", Available: ok && st.Healthy, Detail: "no tool span; action observed in the audit log only"})
	}

	out = append(out, b.watchCoverage(muts))

	ev := model.SourceCoverage{Source: "kubernetes-events"}
	if st, ok := b.v.Source("kubernetes-events"); ok {
		ev.Available = st.Healthy
		ev.Detail = st.Detail
	} else {
		ev.Detail = "events source not reported"
	}
	out = append(out, ev)

	prom := model.SourceCoverage{Source: "prometheus"}
	errs := 0
	for _, m := range metrics {
		if m.Error != "" {
			errs++
		}
	}
	switch {
	case !b.opts.TelemetryConfigured:
		prom.Detail = "no metrics source configured"
	case len(muts) == 0:
		prom.Detail = "no mutation to evaluate"
	case len(metrics) == 0:
		prom.Detail = "telemetry not evaluated yet"
	case errs == len(metrics):
		prom.Detail = "all signal queries failed: " + metrics[0].Error
	default:
		prom.Available = true
		prom.Detail = fmt.Sprintf("%d signal evaluation(s), %d failed", len(metrics), errs)
	}
	out = append(out, prom)
	return out
}

// roundDelta rounds durations for human-readable reasons: milliseconds below
// one second, tenths of a second above.
func roundDelta(d time.Duration) time.Duration {
	if d.Abs() < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}

// watchCoverage reports whether one watch instance observed the whole
// action: it must have started before the action and not restarted before
// the action's windows closed.
func (b *builder) watchCoverage(muts []*mutation) model.SourceCoverage {
	c := model.SourceCoverage{Source: "kubernetes-watch"}
	inst := b.v.SourceInstances("kubernetes-watch")
	if len(inst) == 0 {
		c.Detail = "watch source not reported"
		return c
	}
	start := b.a.StartedAt
	end := start
	for _, m := range muts {
		if m.win.end.After(end) {
			end = m.win.end
		}
	}
	var active time.Time
	restarted := false
	for _, t := range inst {
		switch {
		case !t.After(start):
			active = t
		case !t.After(end):
			restarted = true
		}
	}
	switch {
	case active.IsZero():
		c.Detail = "watch started after the action; earlier changes are unknown"
	case restarted:
		c.Detail = "watch restarted during the action's window; changes during the restart may be missing"
	default:
		c.Available = true
		c.Detail = "watching since " + active.UTC().Format(time.RFC3339)
		if st, ok := b.v.Source("kubernetes-watch"); ok && !st.Healthy {
			c.Available = false
			c.Detail = "watch unhealthy: " + st.Detail
		}
	}
	return c
}
