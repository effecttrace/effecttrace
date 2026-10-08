package correlate

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/pkg/model"
)

// ErrNotFound is returned when an action does not exist.
var ErrNotFound = errors.New("action not found")

// Options are per-engine settings that are not attribution rules.
type Options struct {
	// TelemetryConfigured is true when a metrics source evaluates signals;
	// graphs then wait for telemetry results before reporting COMPLETE.
	TelemetryConfigured bool
}

// Engine builds effect graphs from a store. It caches the action index per
// store version and clock second.
type Engine struct {
	st   *store.Store
	cfg  Config
	opts Options

	mu    sync.Mutex
	cache *index
}

// NewEngine returns an engine over st.
func NewEngine(st *store.Store, cfg Config, opts Options) *Engine {
	return &Engine{st: st, cfg: cfg, opts: opts}
}

// Config returns the attribution configuration.
func (e *Engine) Config() Config { return e.cfg }

// index is the global attribution state for one store version.
type index struct {
	version    uint64
	now        time.Time
	actions    []*action
	byID       map[string]*action
	byAction   map[string][]*mutation
	byUID      map[string][]*mutation
	delByOwner map[string][]*mutation
}

func buildIndex(v store.View, cfg Config, now time.Time) *index {
	ix := &index{
		version:    v.Version(),
		now:        now,
		byID:       map[string]*action{},
		byAction:   map[string][]*mutation{},
		byUID:      map[string][]*mutation{},
		delByOwner: map[string][]*mutation{},
	}
	ix.actions = discover(v, cfg)
	for _, a := range ix.actions {
		ix.byID[a.ID] = a
		for _, r := range a.requests {
			if !successful(r.statusCode()) {
				continue
			}
			m := resolveMutation(v, a, r, cfg, now)
			ix.byAction[a.ID] = append(ix.byAction[a.ID], m)
			// Temporal links do not give an action ownership of a request's
			// effects; the request's own action holds those claims.
			if r.link == linkTemporal || m.uid == "" || !m.changed {
				continue
			}
			ix.byUID[m.uid] = append(ix.byUID[m.uid], m)
			if m.owner != "" && m.kind == "Pod" {
				ix.delByOwner[m.owner] = append(ix.delByOwner[m.owner], m)
			}
		}
	}
	return ix
}

func (e *Engine) withIndex(now time.Time, fn func(v store.View, ix *index)) {
	now = now.Truncate(time.Second)
	e.st.Read(func(v store.View) {
		e.mu.Lock()
		ix := e.cache
		if ix == nil || ix.version != v.Version() || !ix.now.Equal(now) {
			ix = buildIndex(v, e.cfg, now)
			e.cache = ix
		}
		e.mu.Unlock()
		fn(v, ix)
	})
}

// ActionSummary is a compact listing entry.
type ActionSummary struct {
	ID          string                     `json:"id"`
	Kind        model.ActionKind           `json:"kind"`
	Name        string                     `json:"name"`
	Actor       string                     `json:"actor,omitempty"`
	StartedAt   time.Time                  `json:"startedAt"`
	TraceID     string                     `json:"traceId,omitempty"`
	Status      model.GraphStatus          `json:"status"`
	Edges       map[model.EvidenceType]int `json:"edges"`
	Ambiguities int                        `json:"ambiguities"`
}

// Actions lists actions that started in [since, now], oldest first.
func (e *Engine) Actions(now, since time.Time) []ActionSummary {
	var out []ActionSummary
	e.withIndex(now, func(v store.View, ix *index) {
		for _, a := range ix.actions {
			if a.StartedAt.Before(since) {
				continue
			}
			g := build(v, ix, a, e.cfg, e.opts)
			s := ActionSummary{
				ID: a.ID, Kind: a.Kind, Name: a.Name, Actor: a.Actor, StartedAt: a.StartedAt.UTC(),
				Status: g.Status, Edges: map[model.EvidenceType]int{}, Ambiguities: len(g.Ambiguities),
			}
			if a.Trace != nil {
				s.TraceID = a.Trace.TraceID
			}
			for _, ed := range g.Edges {
				s.Edges[ed.Evidence]++
			}
			out = append(out, s)
		}
	})
	return out
}

// Graph builds the effect graph of an action.
func (e *Engine) Graph(now time.Time, actionID string) (*model.EffectGraph, error) {
	var g *model.EffectGraph
	e.withIndex(now, func(v store.View, ix *index) {
		if a, ok := ix.byID[actionID]; ok {
			g = build(v, ix, a, e.cfg, e.opts)
		}
	})
	if g == nil {
		return nil, ErrNotFound
	}
	return g, nil
}

// ActionsForTrace returns the IDs of actions recorded in a trace.
func (e *Engine) ActionsForTrace(now time.Time, traceID string) []string {
	var ids []string
	e.withIndex(now, func(_ store.View, ix *index) {
		for _, a := range ix.actions {
			if a.Trace != nil && a.Trace.TraceID == traceID {
				ids = append(ids, a.ID)
			}
		}
	})
	return ids
}

// TelemetryTarget asks a metrics source to evaluate signals for an action.
type TelemetryTarget struct {
	ActionID      string
	Namespace     string
	BaselineStart time.Time
	WindowStart   time.Time
	WindowEnd     time.Time
	// InScope are workloads reached by a mutation or ownership path.
	InScope []Workload
	// Others are the remaining workloads of the namespace, evaluated only so
	// that changes there can be reported as exclusions.
	Others []Workload
}

// Workload names a scalable workload object.
type Workload struct {
	Kind string
	Name string
	UID  string
}

// PendingTelemetry returns actions whose reconciliation windows have closed
// and whose telemetry window has ended but that have no metric results yet.
func (e *Engine) PendingTelemetry(now time.Time) []TelemetryTarget {
	var out []TelemetryTarget
	e.withIndex(now, func(v store.View, ix *index) {
		for _, a := range ix.actions {
			muts := ix.byAction[a.ID]
			if len(muts) == 0 || len(v.Metrics(a.ID)) > 0 {
				continue
			}
			t, ok := telemetryTarget(v, a, muts, e.cfg)
			if !ok || now.Before(t.WindowEnd) || now.Sub(t.WindowEnd) > e.cfg.MaxTelemetryAge {
				continue
			}
			out = append(out, t)
		}
	})
	// Newest first, so a backlog never delays the most recent actions.
	slices.SortStableFunc(out, func(a, b TelemetryTarget) int { return b.WindowEnd.Compare(a.WindowEnd) })
	return out
}

// telemetryExpired reports whether an action's telemetry window ended too
// long ago to be evaluated.
func telemetryExpired(v store.View, a *action, muts []*mutation, cfg Config, now time.Time) bool {
	t, ok := telemetryTarget(v, a, muts, cfg)
	return ok && now.Sub(t.WindowEnd) > cfg.MaxTelemetryAge
}

func telemetryTarget(v store.View, a *action, muts []*mutation, cfg Config) (TelemetryTarget, bool) {
	t := TelemetryTarget{ActionID: a.ID}
	inScope := map[string]Workload{}
	var start, end time.Time
	for _, m := range muts {
		if m.win.open {
			return TelemetryTarget{}, false
		}
		if start.IsZero() || m.t0.Before(start) {
			start = m.t0
		}
		if m.win.end.After(end) {
			end = m.win.end
		}
		if t.Namespace == "" {
			t.Namespace = m.req.ns
		}
		if w, ok := workloadFor(v, m, cfg); ok {
			inScope[w.UID] = w
		}
	}
	if t.Namespace == "" {
		return TelemetryTarget{}, false
	}
	t.BaselineStart = start.Add(-cfg.BaselineWindow)
	t.WindowStart = start
	t.WindowEnd = end.Add(cfg.TelemetrySettle)
	for _, w := range inScope {
		t.InScope = append(t.InScope, w)
	}
	for _, h := range v.ObjectsInNamespace(t.Namespace) {
		if !isWorkloadKind(h.Ref.Kind) {
			continue
		}
		if _, in := inScope[h.Ref.UID]; in {
			continue
		}
		if d := h.DeletedAt(); !d.IsZero() && d.Before(t.BaselineStart) {
			continue
		}
		t.Others = append(t.Others, Workload{Kind: h.Ref.Kind, Name: h.Ref.Name, UID: h.Ref.UID})
	}
	sortWorkloads(t.InScope)
	sortWorkloads(t.Others)
	return t, true
}

func sortWorkloads(ws []Workload) {
	slices.SortFunc(ws, func(a, b Workload) int { return strings.Compare(a.Name+"/"+a.UID, b.Name+"/"+b.UID) })
}

func isWorkloadKind(k string) bool {
	switch k {
	case "Deployment", "StatefulSet", "DaemonSet":
		return true
	}
	return false
}

// workloadFor returns the top-level workload whose telemetry a mutation can
// structurally reach: the mutated workload itself, or the workload owning a
// mutated ReplicaSet or deleted Pod.
func workloadFor(v store.View, m *mutation, cfg Config) (Workload, bool) {
	if m.uid == "" {
		return Workload{}, false
	}
	chain := append([]string{m.uid}, ancestors(v, m.uid, cfg.MaxDepth)...)
	for i := len(chain) - 1; i >= 0; i-- {
		h, ok := v.Object(chain[i])
		if ok && isWorkloadKind(h.Ref.Kind) {
			return Workload{Kind: h.Ref.Kind, Name: h.Ref.Name, UID: h.Ref.UID}, true
		}
	}
	return Workload{}, false
}
