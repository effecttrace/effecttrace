package experiments

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/effecttrace/effecttrace/pkg/model"
)

// Env is shared by scenarios.
type Env struct {
	Lab *Lab
	Rec *Recorder
	Log func(string, ...any)
	// Results of earlier scenarios, for replay experiments.
	Prior map[string]*Result
}

// Check is one pass/fail assertion.
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Outcome is one action of a scenario.
type Outcome struct {
	Label     string     `json:"label"`
	ActionID  string     `json:"actionId"`
	Kind      string     `json:"kind"`
	TraceID   string     `json:"traceId,omitempty"`
	Started   time.Time  `json:"started"`
	Returned  time.Time  `json:"returned"`
	End       time.Time  `json:"truthEnd"`
	Truth     Truth      `json:"truth"`
	Unrelated int        `json:"unrelatedChangedObjects"`
	Eval      Evaluation `json:"evaluation"`
	// FirstEffectS is not reported: the harness starts polling only after
	// the workload stabilizes, so it would measure time to the first poll.
	FirstEffectS float64            `json:"-"`
	CompleteS    float64            `json:"completeSeconds,omitempty"`
	QueryMS      []float64          `json:"queryMilliseconds,omitempty"`
	Graph        *model.EffectGraph `json:"-"`
	unrelatedIDs []string
}

// Result is the outcome of one scenario.
type Result struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Category    string     `json:"category"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	Started     time.Time  `json:"started"`
	DurationS   float64    `json:"durationSeconds"`
	Actions     []*Outcome `json:"actions"`
	Checks      []Check    `json:"checks"`
	Notes       []string   `json:"notes,omitempty"`
	Error       string     `json:"error,omitempty"`
	// Concurrent marks scenarios whose actions must be separated.
	Concurrent bool  `json:"concurrent"`
	Separated  *bool `json:"separated,omitempty"`
}

// Scenario is one experiment.
type Scenario struct {
	ID          string
	Title       string
	Category    string // live, replay, unsupported
	Description string
	// Quiet requests a longer quiet period first so that the telemetry
	// baseline is not disturbed by the previous scenario's cleanup.
	Quiet bool
	Run   func(ctx context.Context, e *Env, r *Result) error
}

func (r *Result) check(name string, pass bool, format string, a ...any) {
	r.Checks = append(r.Checks, Check{Name: name, Pass: pass, Detail: fmt.Sprintf(format, a...)})
}

func (r *Result) note(format string, a ...any) { r.Notes = append(r.Notes, fmt.Sprintf(format, a...)) }

// ---- actions ----------------------------------------------------------

func (e *Env) mcp(ctx context.Context, label, tool string, args map[string]any, dropContext bool) (*Outcome, error) {
	o := &Outcome{Label: label, Kind: string(model.ActionMCPToolCall), Started: time.Now()}
	res, err := e.Lab.MCP(ctx, tool, args, dropContext)
	o.Returned = time.Now()
	o.TraceID = res.TraceID
	if err != nil {
		return o, fmt.Errorf("%s: %w", label, err)
	}
	return o, nil
}

func (e *Env) resolveMCP(ctx context.Context, o *Outcome) error {
	id, err := e.Lab.ActionForTrace(ctx, o.TraceID, 60*time.Second)
	if err != nil {
		return fmt.Errorf("%s: %w", o.Label, err)
	}
	o.ActionID = id
	return nil
}

func (e *Env) kubectl(ctx context.Context, label string, args ...string) (*Outcome, error) {
	o := &Outcome{Label: label, Kind: string(model.ActionKubernetesAPICall), Started: time.Now()}
	ids, _, err := e.Lab.Kubectl(ctx, args...)
	o.Returned = time.Now()
	if err != nil {
		return o, err
	}
	id, err := e.Lab.ActionForAudit(ctx, ids, 60*time.Second)
	if err != nil {
		return o, fmt.Errorf("%s: %w", label, err)
	}
	o.ActionID = id
	return o, nil
}

// waitStable waits until the named Deployments report a completed rollout.
func (e *Env) waitStable(ctx context.Context, timeout time.Duration, names ...string) error {
	deadline := time.Now().Add(timeout)
	for {
		all := true
		for _, n := range names {
			d, err := e.Lab.Client.AppsV1().Deployments(Namespace).Get(ctx, n, metav1.GetOptions{})
			if err != nil || !stable(d) {
				all = false
				break
			}
		}
		if all {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployments %v not stable within %s", names, timeout)
		}
		if err := pause(ctx, time.Second); err != nil {
			return err
		}
	}
}

func stable(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.UpdatedReplicas == want && s.AvailableReplicas == want && s.Replicas == want
}

// settle waits for terminations and Events after a stable status. It
// matches the collector's Settle duration plus margin.
func settle(ctx context.Context) error { return pause(ctx, 7*time.Second) }

// ---- evaluation -------------------------------------------------------

// finish waits for every action's graph, computes truths with fn, and
// evaluates.
func (e *Env) finish(ctx context.Context, r *Result, timeout time.Duration, truths func(objs map[string]Life) error, outs ...*Outcome) error {
	for _, o := range outs {
		if o.ActionID == "" && o.TraceID != "" {
			if err := e.resolveMCP(ctx, o); err != nil {
				return err
			}
		}
	}
	for _, o := range outs {
		gp, err := e.Lab.WaitComplete(ctx, o.ActionID, o.Started, timeout)
		o.Graph = gp.Graph
		o.FirstEffectS = gp.FirstEffect.Seconds()
		o.CompleteS = gp.Complete.Seconds()
		for _, q := range gp.QueryLatencies {
			o.QueryMS = append(o.QueryMS, float64(q.Microseconds())/1000)
		}
		if err != nil {
			return err
		}
	}
	objs := e.Rec.Snapshot()
	if err := truths(objs); err != nil {
		return err
	}
	var lo, hi time.Time
	for _, o := range outs {
		if lo.IsZero() || o.Started.Before(lo) {
			lo = o.Started
		}
		if o.End.After(hi) {
			hi = o.End
		}
	}
	changed := ChangedInNamespace(objs, lo, hi)
	for _, o := range outs {
		mine := append(append(append(append([]string{}, o.Truth.Direct...), o.Truth.Structural...), o.Truth.Ambiguous...), o.Truth.Uncertain...)
		for _, u := range changed {
			if !slices.Contains(mine, u) {
				o.unrelatedIDs = append(o.unrelatedIDs, u)
			}
		}
		// Objects expected for other actions are unrelated to this one.
		for _, other := range outs {
			if other == o {
				continue
			}
			for _, u := range append(append([]string{}, other.Truth.Direct...), other.Truth.Structural...) {
				if !slices.Contains(mine, u) && !slices.Contains(o.unrelatedIDs, u) {
					o.unrelatedIDs = append(o.unrelatedIDs, u)
				}
			}
		}
		o.Unrelated = len(o.unrelatedIDs)
		o.Eval = Evaluate(o.Graph, o.Truth, o.unrelatedIDs)
		r.Actions = append(r.Actions, o)
	}
	return nil
}

// standardChecks asserts the attribution properties every action must have.
func standardChecks(r *Result, o *Outcome) {
	ev := o.Eval
	r.check(o.Label+": no false attachments", len(ev.FalseAttachments) == 0, "%v", ev.FalseAttachments)
	r.check(o.Label+": no attributed claim on an ambiguous change", len(ev.AmbiguousClaimed) == 0, "%v", ev.AmbiguousClaimed)
	r.check(o.Label+": direct precision", ev.Direct.FP == 0, "tp=%d fp=%d fn=%d", ev.Direct.TP, ev.Direct.FP, ev.Direct.FN)
	r.check(o.Label+": direct recall", ev.Direct.FN == 0, "tp=%d fp=%d fn=%d", ev.Direct.TP, ev.Direct.FP, ev.Direct.FN)
	r.check(o.Label+": structural precision", ev.Structural.FP == 0, "tp=%d fp=%d fn=%d", ev.Structural.TP, ev.Structural.FP, ev.Structural.FN)
	r.check(o.Label+": structural recall", ev.Structural.FN == 0, "tp=%d fp=%d fn=%d", ev.Structural.TP, ev.Structural.FP, ev.Structural.FN)
	r.check(o.Label+": no unrelated telemetry attached", len(ev.UnrelatedTelemetry) == 0, "%v", ev.UnrelatedTelemetry)
}

// separation records whether concurrent actions claimed each other's
// objects.
func separation(r *Result) {
	r.Concurrent = true
	ok := true
	for _, a := range r.Actions {
		for _, b := range r.Actions {
			if a == b {
				continue
			}
			for _, u := range a.Eval.Claims {
				if slices.Contains(b.Truth.Direct, u) || slices.Contains(b.Truth.Structural, u) {
					if !slices.Contains(a.Truth.Direct, u) && !slices.Contains(a.Truth.Structural, u) {
						ok = false
					}
				}
			}
		}
	}
	r.Separated = &ok
	r.check("concurrent actions are separated", ok, "no action claimed another action's expected objects")
}

// workloadTruth is the truth for an action on one Deployment.
func workloadTruth(objs map[string]Life, o *Outcome, uid, name string) {
	o.Truth = Truth{Direct: []string{uid}, Structural: DescendantTruth(objs, uid, o.Started, o.End), Workloads: []string{name}}
}

func hasEdge(g *model.EffectGraph, ev model.EvidenceType, labelContains string) bool {
	nodes := map[string]model.Node{}
	for _, n := range g.Nodes {
		nodes[n.ID] = n
	}
	for _, e := range g.Edges {
		if e.Evidence == ev && strings.Contains(nodes[e.To].Label, labelContains) {
			return true
		}
	}
	return false
}

func coverage(g *model.EffectGraph, source string) model.SourceCoverage {
	for _, c := range g.Coverage {
		if c.Source == source {
			return c
		}
	}
	return model.SourceCoverage{}
}

// ---- baseline ---------------------------------------------------------

// Restore returns the shop namespace to its declared state.
func (e *Env) Restore(ctx context.Context) error {
	if _, err := e.Lab.KubectlQuiet(ctx, "apply", "-f", e.Lab.Repo+"/deploy/lab/shop.yaml"); err != nil {
		return err
	}
	names := []string{"frontend", "checkout", "payments", "inventory"}
	if err := e.waitStable(ctx, 4*time.Minute, names...); err != nil {
		return err
	}
	return settle(ctx)
}
