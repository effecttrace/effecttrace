package correlate

import (
	"slices"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/pkg/model"
)

// resolution describes how a request's target UID was determined.
type resolution string

const (
	resolvedResponse resolution = "response-metadata"    // UID returned to the instrumented client
	resolvedAudit    resolution = "audit-object-ref"     // UID in the audit objectRef
	resolvedName     resolution = "name-at-request-time" // unique object with that name at request time
	resolvedNone     resolution = "unresolved"
	resolvedAmbig    resolution = "ambiguous-name"
)

// window is an attribution interval.
type window struct {
	start, end time.Time
	open       bool
	reason     string
}

func (w window) contains(t time.Time) bool {
	return !t.Before(w.start) && !t.After(w.end)
}

// mutation is a resolved direct change made by one request of one action.
type mutation struct {
	act        *action
	req        *request
	uid        string
	kind       string
	how        resolution
	change     string // MUTATED, CREATED, DELETED
	genFrom    int64
	genTo      int64
	rv         string
	replFrom   *int32
	replTo     *int32
	observedAt time.Time
	t0         time.Time
	win        window
	// owner is the controller owner of a deleted Pod, whose replacement
	// Pods fall in the mutation's scope.
	owner string
	// changed is true when the watch observed the change this request
	// made: a generation increase, a creation or a deletion. Only changed
	// mutations are given controller fan-out; a no-op request (for example
	// a kubectl apply that changed nothing) never claims later effects.
	changed bool
}

// generationKinds are kinds whose spec changes increase metadata.generation.
var generationKinds = map[string]bool{"Deployment": true, "ReplicaSet": true, "StatefulSet": true, "DaemonSet": true, "Job": true}

// resolveTarget determines the UID a request mutated.
func resolveTarget(v store.View, r *request, cfg Config) (string, resolution) {
	if r.respUID != "" {
		return r.respUID, resolvedResponse
	}
	if r.audit != nil && r.audit.ObjectUID != "" {
		return r.audit.ObjectUID, resolvedAudit
	}
	if r.name == "" {
		return "", resolvedNone
	}
	var matches []string
	for _, uid := range v.ObjectsByName(r.kind, r.ns, r.name) {
		h, ok := v.Object(uid)
		if !ok {
			continue
		}
		created := h.CreatedAt()
		deleted := h.DeletedAt()
		switch r.verb {
		case "create":
			// The object this request created appears after the request.
			if !created.Before(r.at.Add(-cfg.SkewTolerance)) && created.Before(r.at.Add(cfg.Settle)) {
				matches = append(matches, uid)
			}
		default:
			alive := !created.After(r.at.Add(cfg.SkewTolerance)) &&
				(deleted.IsZero() || !deleted.Before(r.at.Add(-cfg.SkewTolerance)))
			if alive {
				matches = append(matches, uid)
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", resolvedNone
	case 1:
		return matches[0], resolvedName
	default:
		return "", resolvedAmbig
	}
}

func changeForVerb(verb string) string {
	switch verb {
	case "create":
		return "CREATED"
	case "delete", "deletecollection":
		return "DELETED"
	default:
		return "MUTATED"
	}
}

// resolveMutation builds the mutation record for a successful request.
func resolveMutation(v store.View, a *action, r *request, cfg Config, now time.Time) *mutation {
	uid, how := resolveTarget(v, r, cfg)
	m := &mutation{act: a, req: r, uid: uid, kind: r.kind, how: how, change: changeForVerb(r.verb), t0: r.at, rv: r.respRV}
	m.genTo = r.respGen
	h, ok := v.Object(uid)
	if uid == "" || !ok {
		m.win = window{start: r.at.Add(-cfg.SkewTolerance), end: r.at.Add(cfg.Settle), reason: "target object not observed; no structural fan-out followed"}
		m.win.open = now.Before(m.win.end)
		return m
	}
	m.kind = h.Ref.Kind
	if owner, ok := h.ControllerOwner(); ok && m.change == "DELETED" {
		m.owner = owner.UID
	}
	idx := locateObservation(h, m, cfg)
	switch {
	case idx < 0:
		m.changed = false
	case m.change == "MUTATED" && generationKinds[m.kind]:
		m.changed = m.genTo > m.genFrom && m.genFrom > 0
	default:
		m.changed = true
	}
	if !m.changed {
		m.win = window{start: r.at.Add(-cfg.SkewTolerance), end: r.at.Add(cfg.Settle),
			reason: "no resulting spec change, creation or deletion was observed for this request; no controller fan-out is attributed"}
		m.win.open = now.Before(m.win.end)
		return m
	}
	m.win = reconcileWindow(v, h, m, cfg, now)
	return m
}

// observeBound is how long after a request completes its own watch
// notification is expected. Notifications later than this are not
// attributed to an audit-only request, because they may belong to a later
// request.
const observeBound = time.Second

// locateObservation finds the watch notification produced by the mutation
// and returns its index, or -1. With a resourceVersion reported by the
// instrumented client the match is exact. Otherwise it is the first
// notification between the request and shortly after its completion.
func locateObservation(h *store.ObjectHistory, m *mutation, cfg Config) int {
	obsList := h.Observations
	idx := -1
	if m.rv != "" {
		for i, o := range obsList {
			if o.ResourceVersion == m.rv {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		lo := m.t0.Add(-cfg.SkewTolerance)
		hi := m.t0.Add(observeBound)
		if m.req.audit != nil && m.req.audit.CompletedAt.After(m.t0) {
			hi = m.req.audit.CompletedAt.Add(observeBound)
		}
		if m.change == "DELETED" {
			// Graceful deletion is observed as soon as deletionTimestamp is
			// set; foreground deletion may take until dependents are gone.
			hi = hi.Add(cfg.Settle)
		}
		for i, o := range obsList {
			if o.Initial || o.At.Before(lo) || o.At.After(hi) {
				continue
			}
			if m.change == "DELETED" && o.Type != obs.WatchDeleted && o.DeletingAt.IsZero() {
				continue
			}
			if m.change == "CREATED" && i != 0 {
				continue
			}
			if m.change == "MUTATED" && generationKinds[h.Ref.Kind] && (i == 0 || o.Generation <= obsList[i-1].Generation) {
				// Status updates by controllers do not change generation.
				continue
			}
			idx = i
			break
		}
	}
	if idx < 0 {
		return -1
	}
	o := obsList[idx]
	m.observedAt = o.At
	if m.genTo == 0 {
		m.genTo = o.Generation
	}
	if m.rv == "" {
		m.rv = o.ResourceVersion
	}
	m.replTo = o.Replicas
	if idx > 0 {
		prev := obsList[idx-1]
		m.genFrom = prev.Generation
		m.replFrom = prev.Replicas
	}
	if m.replFrom != nil && m.replTo != nil && *m.replFrom == *m.replTo {
		m.replFrom, m.replTo = nil, nil
	}
	return idx
}

// reconcileWindow computes how long the controller-driven effects of a
// mutation are attributed to it: until the owning workload reports a stable
// status, plus Settle, capped at MaxReconcile.
func reconcileWindow(v store.View, h *store.ObjectHistory, m *mutation, cfg Config, now time.Time) window {
	w := window{start: m.t0.Add(-cfg.SkewTolerance)}
	limit := m.t0.Add(cfg.MaxReconcile)
	stable, ok := stableAt(v, h, m)
	switch {
	case ok:
		w.end = stable.Add(cfg.Settle)
		if w.end.After(limit) {
			w.end = limit
		}
		w.reason = "closed " + cfg.Settle.String() + " after " + m.kind + " reported a stable status"
	default:
		w.end = limit
		w.reason = "capped at max reconcile duration " + cfg.MaxReconcile.String() + "; " + m.kind + " did not report a stable status"
	}
	if w.end.Before(m.t0) {
		w.end = m.t0
	}
	w.open = now.Before(w.end)
	if w.open && !ok {
		w.reason = "open: waiting for " + m.kind + " to report a stable status (max " + cfg.MaxReconcile.String() + ")"
	}
	return w
}

func eq(a, b *int32) bool { return a != nil && b != nil && *a == *b }

// stableAt returns the first observation time at which the workload affected
// by the mutation reported a converged status.
func stableAt(v store.View, h *store.ObjectHistory, m *mutation) (time.Time, bool) {
	after := m.t0
	switch m.kind {
	case "Deployment":
		for _, o := range h.Observations {
			if o.At.Before(after) || o.Initial {
				continue
			}
			if o.ObservedGeneration >= m.genTo && eq(o.UpdatedReplicas, o.Replicas) &&
				eq(o.AvailableReplicas, o.Replicas) && eq(o.StatusReplicas, o.Replicas) {
				return o.At, true
			}
			if o.Replicas != nil && *o.Replicas == 0 && o.ObservedGeneration >= m.genTo &&
				(o.StatusReplicas == nil || *o.StatusReplicas == 0) {
				return o.At, true
			}
		}
	case "StatefulSet", "ReplicaSet":
		for _, o := range h.Observations {
			if o.At.Before(after) || o.Initial {
				continue
			}
			if o.ObservedGeneration >= m.genTo && eq(o.ReadyReplicas, o.Replicas) &&
				(m.kind == "ReplicaSet" || eq(o.UpdatedReplicas, o.Replicas)) {
				return o.At, true
			}
			if o.Replicas != nil && *o.Replicas == 0 && o.ObservedGeneration >= m.genTo {
				return o.At, true
			}
		}
	case "Pod":
		if m.change != "DELETED" {
			return m.t0, true
		}
		deleted := h.DeletedAt()
		if deleted.IsZero() {
			return time.Time{}, false
		}
		if m.owner == "" {
			return deleted, true
		}
		oh, ok := v.Object(m.owner)
		if !ok {
			return deleted, true
		}
		for _, o := range oh.Observations {
			if o.At.Before(deleted) || o.Initial {
				continue
			}
			if eq(o.ReadyReplicas, o.Replicas) {
				return o.At, true
			}
		}
	default:
		if !m.observedAt.IsZero() {
			return m.observedAt, true
		}
		return m.t0, true
	}
	return time.Time{}, false
}

// ancestors returns the controller-owner chain of uid, nearest first.
func ancestors(v store.View, uid string, maxDepth int) []string {
	var out []string
	cur := uid
	for range maxDepth {
		h, ok := v.Object(cur)
		if !ok {
			break
		}
		o, ok := h.ControllerOwner()
		if !ok || slices.Contains(out, o.UID) || o.UID == uid {
			break
		}
		out = append(out, o.UID)
		cur = o.UID
	}
	return out
}

// refOf returns the object reference of a history.
func refOf(h *store.ObjectHistory) model.ObjectRef {
	return h.Ref
}
