package correlate_test

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/internal/synth"
	"github.com/effecttrace/effecttrace/pkg/model"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func engineFor(t testing.TB, recs []obs.Record) (*correlate.Engine, time.Time) {
	t.Helper()
	st := store.New(store.DefaultConfig())
	var last time.Time
	for _, r := range recs {
		if _, err := st.Apply(r); err != nil {
			t.Fatalf("apply %s: %v", r.Kind, err)
		}
	}
	st.Read(func(v store.View) { last = v.Newest() })
	return correlate.NewEngine(st, correlate.DefaultConfig(), correlate.Options{}), last.Add(10 * time.Minute)
}

func graphFor(t testing.TB, e *correlate.Engine, now time.Time, match func(correlate.ActionSummary) bool) *model.EffectGraph {
	t.Helper()
	for _, a := range e.Actions(now, time.Time{}) {
		if match(a) {
			g, err := e.Graph(now, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Validate(); err != nil {
				t.Fatalf("graph invalid: %v", err)
			}
			return g
		}
	}
	t.Fatalf("no matching action among %v", e.Actions(now, time.Time{}))
	return nil
}

func byKind(k model.ActionKind) func(correlate.ActionSummary) bool {
	return func(a correlate.ActionSummary) bool { return a.Kind == k }
}

func nodeByUID(g *model.EffectGraph, uid string) *model.Node {
	for i := range g.Nodes {
		if g.Nodes[i].Object != nil && g.Nodes[i].Object.UID == uid {
			return &g.Nodes[i]
		}
	}
	return nil
}

func edgesTo(g *model.EffectGraph, id string) []model.Edge {
	var out []model.Edge
	for _, e := range g.Edges {
		if e.To == id {
			out = append(out, e)
		}
	}
	return out
}

func restartScenario(c *synth.Cluster, w *synth.Workload, opt synth.MCPOptions) {
	_, finish := c.MCPCall("restart_workload", synth.Request{Verb: "patch", Res: "deployments", NS: w.Deploy.Ref.Namespace, Name: w.Deploy.Ref.Name}, w.Deploy, opt)
	w.Mutate()
	finish()
	w.Rollout(false)
}

func TestRestartAttributesRolloutDescendants(t *testing.T) {
	c := synth.New(t0)
	c.Sources()
	c.Advance(time.Minute)
	w := c.Deployment("shop", "checkout", 3)
	oldRS := w.Current
	oldPods := slices.Clone(w.Pods[oldRS])
	c.Advance(time.Minute)
	restartScenario(c, w, synth.MCPOptions{})

	e, now := engineFor(t, c.Records())
	g := graphFor(t, e, now, byKind(model.ActionMCPToolCall))

	if g.Status != model.StatusComplete {
		t.Errorf("status = %s, want COMPLETE", g.Status)
	}
	dep := nodeByUID(g, w.Deploy.Ref.UID)
	if dep == nil || dep.Change == nil || dep.Change.Type != "MUTATED" {
		t.Fatalf("deployment node missing or not MUTATED: %+v", dep)
	}
	if dep.Change.GenerationFrom != 1 || dep.Change.GenerationTo != 2 {
		t.Errorf("generation %d -> %d, want 1 -> 2", dep.Change.GenerationFrom, dep.Change.GenerationTo)
	}
	in := edgesTo(g, dep.ID)
	if len(in) != 1 || in[0].Evidence != model.EvidenceDirect || in[0].Rule != correlate.RuleResponseUID {
		t.Fatalf("deployment incoming edges = %+v, want one DIRECT via response UID", in)
	}
	if !strings.Contains(in[0].Reason, "independently confirms") {
		t.Errorf("direct edge reason lacks audit confirmation: %q", in[0].Reason)
	}
	newRS := nodeByUID(g, w.Current.Ref.UID)
	if newRS == nil || newRS.Change.Type != "CREATED" || newRS.Grade != model.GradeAttributed {
		t.Fatalf("new ReplicaSet not attributed: %+v", newRS)
	}
	for _, p := range w.Pods[w.Current] {
		n := nodeByUID(g, p.Ref.UID)
		if n == nil || n.Change.Type != "CREATED" || n.Grade != model.GradeAttributed {
			t.Errorf("new pod %s not attributed: %+v", p.Ref.Name, n)
		}
		if n != nil && n.Change.ReadyAt.IsZero() {
			t.Errorf("new pod %s has no readyAt", p.Ref.Name)
		}
	}
	for _, p := range oldPods {
		n := nodeByUID(g, p.Ref.UID)
		if n == nil || n.Change.Type != "DELETED" {
			t.Errorf("old pod %s not attributed as DELETED: %+v", p.Ref.Name, n)
		}
	}
	if n := nodeByUID(g, oldRS.Ref.UID); n == nil || n.Change == nil || n.Change.Type != "SCALED" {
		t.Errorf("old ReplicaSet not attributed as SCALED: %+v", n)
	}
	events := 0
	for _, ed := range g.Edges {
		if ed.Evidence == model.EvidenceEventReference {
			events++
		}
	}
	if events == 0 {
		t.Error("no EVENT_REFERENCE edges")
	}
	for _, ed := range g.Edges {
		if ed.Evidence == model.EvidenceTemporalCorrelation {
			t.Errorf("unexpected temporal edge without telemetry: %+v", ed)
		}
	}
	if len(g.Ambiguities) != 0 {
		t.Errorf("unexpected ambiguities: %+v", g.Ambiguities)
	}
}

func TestUnrelatedConcurrentChangeIsExcluded(t *testing.T) {
	c := synth.New(t0)
	c.Sources()
	checkout := c.Deployment("shop", "checkout", 3)
	payments := c.Deployment("shop", "payments", 2)
	c.Advance(time.Minute)

	_, finish := c.MCPCall("restart_workload", synth.Request{Verb: "patch", Res: "deployments", NS: "shop", Name: "checkout"}, checkout.Deploy, synth.MCPOptions{})
	checkout.Mutate()
	finish()
	// A human scales payments 200ms later; both reconcile concurrently.
	c.Advance(200 * time.Millisecond)
	c.Audit(synth.Request{AuditID: "human-1", Verb: "patch", Res: "deployments", Sub: "scale", NS: "shop", Name: "payments", User: "kubernetes-admin", At: c.Now})
	payments.Mutate()
	payments.Scale(4)
	checkout.Rollout(false)

	e, now := engineFor(t, c.Records())
	g := graphFor(t, e, now, byKind(model.ActionMCPToolCall))
	for _, n := range g.Nodes {
		if n.Object != nil && strings.HasPrefix(n.Object.Name, "payments") {
			t.Errorf("payments object attached to checkout action: %s", n.Label)
		}
	}
	claimed := 0
	for _, x := range g.Exclusions {
		if strings.Contains(x.Label, "payments") && x.ClaimedBy == "k8s-human-1" {
			claimed++
		}
	}
	if claimed == 0 {
		t.Errorf("payments changes not reported as exclusions claimed by the human action: %+v", g.Exclusions)
	}

	h := graphFor(t, e, now, byKind(model.ActionKubernetesAPICall))
	for _, n := range h.Nodes {
		if n.Object != nil && strings.HasPrefix(n.Object.Name, "checkout") {
			t.Errorf("checkout object attached to payments action: %s", n.Label)
		}
	}
	pd := nodeByUID(h, payments.Deploy.Ref.UID)
	if pd == nil {
		t.Fatal("payments deployment not in human action graph")
	}
	if in := edgesTo(h, pd.ID); len(in) != 1 || in[0].Rule != correlate.RuleNameAtRequestTime {
		t.Errorf("human action edge = %+v, want name-resolved DIRECT", in)
	}
}

func TestSameWorkloadConcurrentActionsAreAmbiguous(t *testing.T) {
	c := synth.New(t0)
	c.Sources()
	w := c.Deployment("shop", "checkout", 3)
	c.Advance(time.Minute)
	_, finish := c.MCPCall("restart_workload", synth.Request{Verb: "patch", Res: "deployments", NS: "shop", Name: "checkout"}, w.Deploy, synth.MCPOptions{})
	w.Mutate()
	finish()
	c.Advance(300 * time.Millisecond)
	c.Audit(synth.Request{AuditID: "human-2", Verb: "patch", Res: "deployments", NS: "shop", Name: "checkout", User: "kubernetes-admin", At: c.Now})
	w.Mutate()
	w.Rollout(false)

	e, now := engineFor(t, c.Records())
	a := graphFor(t, e, now, byKind(model.ActionMCPToolCall))
	b := graphFor(t, e, now, byKind(model.ActionKubernetesAPICall))
	if len(a.Ambiguities) == 0 || len(b.Ambiguities) == 0 {
		t.Fatalf("expected ambiguities in both graphs, got %d and %d", len(a.Ambiguities), len(b.Ambiguities))
	}
	for _, p := range w.Pods[w.Current] {
		if nodeByUID(a, p.Ref.UID) != nil && nodeByUID(b, p.Ref.UID) != nil {
			t.Errorf("pod %s attached to both actions", p.Ref.Name)
		}
	}
	// No change after the second request may be attributed to the first.
	second := b.Action.StartedAt
	for _, n := range a.Nodes {
		if n.Change == nil || n.Object == nil || n.Object.Kind == "Deployment" {
			continue
		}
		if n.Change.At.After(second) || n.Change.DeletedAt.After(second) {
			t.Errorf("first action claims a change after the second request: %s %+v", n.Label, n.Change)
		}
	}
}

func TestMissingTraceLinkDegradesToCorrelation(t *testing.T) {
	c := synth.New(t0)
	c.Sources()
	w := c.Deployment("shop", "checkout", 2)
	c.Advance(time.Minute)
	restartScenario(c, w, synth.MCPOptions{DropTraceLink: true})

	e, now := engineFor(t, c.Records())
	g := graphFor(t, e, now, byKind(model.ActionMCPToolCall))
	root := model.ActionNodeID(g.Action.ID)
	for _, ed := range g.Edges {
		if ed.From == root && ed.Evidence != model.EvidenceTemporalCorrelation {
			t.Errorf("action edge %s carries %s, want TEMPORAL_CORRELATION only", ed.Relationship, ed.Evidence)
		}
	}
	dep := nodeByUID(g, w.Deploy.Ref.UID)
	if dep == nil || dep.Grade != model.GradeCorrelated {
		t.Fatalf("deployment should be reachable only as CORRELATED: %+v", dep)
	}
	for _, p := range w.Pods[w.Current] {
		if n := nodeByUID(g, p.Ref.UID); n == nil || n.Grade != model.GradeCorrelated {
			t.Errorf("new pod should be CORRELATED in the tool graph: %+v", n)
		}
	}
	// The request itself remains an attributed API-call action.
	api := graphFor(t, e, now, byKind(model.ActionKubernetesAPICall))
	if n := nodeByUID(api, w.Deploy.Ref.UID); n == nil || n.Grade != model.GradeAttributed {
		t.Errorf("API-call action should attribute the deployment: %+v", n)
	}
	if !slices.Contains(g.Notes, "Temporal correlation does not prove causation.") {
		t.Errorf("missing causation note: %v", g.Notes)
	}
}

func TestMissingAuditStillDirectFromResponse(t *testing.T) {
	c := synth.New(t0)
	c.Sources()
	w := c.Deployment("shop", "checkout", 2)
	c.Advance(time.Minute)
	restartScenario(c, w, synth.MCPOptions{DropAudit: true})
	e, now := engineFor(t, c.Records())
	g := graphFor(t, e, now, byKind(model.ActionMCPToolCall))
	dep := nodeByUID(g, w.Deploy.Ref.UID)
	if dep == nil {
		t.Fatal("deployment missing")
	}
	in := edgesTo(g, dep.ID)
	if len(in) != 1 || in[0].Evidence != model.EvidenceDirect {
		t.Fatalf("want DIRECT edge, got %+v", in)
	}
	for _, cv := range g.Coverage {
		if cv.Source == "kubernetes-audit" && cv.Available {
			t.Errorf("audit coverage should be unavailable: %+v", cv)
		}
	}
	for _, a := range e.Actions(now, time.Time{}) {
		if a.Kind == model.ActionKubernetesAPICall {
			t.Errorf("unexpected API action without audit: %+v", a)
		}
	}
}

func TestSameNameRecreatedDoesNotCreateFalseContinuity(t *testing.T) {
	c := synth.New(t0)
	c.Sources()
	first := c.Deployment("shop", "checkout", 2)
	c.Advance(time.Minute)
	// Delete the first Deployment and its children.
	c.Audit(synth.Request{AuditID: "del-1", Verb: "delete", Res: "deployments", NS: "shop", Name: "checkout", User: "kubernetes-admin", At: c.Now})
	first.Delete()
	// Recreate with the same name and a new UID.
	c.Advance(10 * time.Second)
	c.Audit(synth.Request{AuditID: "create-2", Verb: "create", Res: "deployments", NS: "shop", User: "kubernetes-admin", At: c.Now})
	second := c.CreateDeployment("shop", "checkout", 2)
	c.Advance(time.Minute)
	c.Audit(synth.Request{AuditID: "patch-2", Verb: "patch", Res: "deployments", NS: "shop", Name: "checkout", User: "kubernetes-admin", At: c.Now})
	second.Mutate()
	second.Rollout(false)

	e, now := engineFor(t, c.Records())
	g, err := e.Graph(now, "k8s-patch-2")
	if err != nil {
		t.Fatal(err)
	}
	if nodeByUID(g, first.Deploy.Ref.UID) != nil {
		t.Error("graph attached the deleted Deployment with the same name")
	}
	if nodeByUID(g, second.Deploy.Ref.UID) == nil {
		t.Error("graph did not attach the recreated Deployment")
	}
	for _, p := range first.Pods[first.Current] {
		if nodeByUID(g, p.Ref.UID) != nil {
			t.Errorf("pod %s of the deleted Deployment attached", p.Ref.Name)
		}
	}
	// The delete action attributes the old objects, never the new ones.
	d, err := e.Graph(now, "k8s-del-1")
	if err != nil {
		t.Fatal(err)
	}
	if nodeByUID(d, first.Deploy.Ref.UID) == nil {
		t.Error("delete action did not attach the deleted Deployment")
	}
	if nodeByUID(d, second.Deploy.Ref.UID) != nil {
		t.Error("delete action attached the recreated Deployment")
	}
	for _, p := range first.Pods[first.Current] {
		if n := nodeByUID(d, p.Ref.UID); n == nil || n.Change.Type != "DELETED" {
			t.Errorf("garbage-collected pod %s not attributed to delete: %+v", p.Ref.Name, n)
		}
	}
}

func TestDeletePodAttributesReplacement(t *testing.T) {
	c := synth.New(t0)
	c.Sources()
	w := c.Deployment("shop", "checkout", 3)
	c.Advance(time.Minute)
	victim := w.Pod(0)
	_, finish := c.MCPCall("delete_pod", synth.Request{Verb: "delete", Res: "pods", NS: "shop", Name: victim.Ref.Name}, victim, synth.MCPOptions{})
	w.DeletePod()
	finish()
	c.Advance(2 * time.Second)

	e, now := engineFor(t, c.Records())
	g := graphFor(t, e, now, byKind(model.ActionMCPToolCall))
	vn := nodeByUID(g, victim.Ref.UID)
	if vn == nil || vn.Change.Type != "DELETED" {
		t.Fatalf("victim pod not DELETED: %+v", vn)
	}
	pods := w.Pods[w.Current]
	repl := pods[len(pods)-1]
	rn := nodeByUID(g, repl.Ref.UID)
	if rn == nil || rn.Change.Type != "CREATED" || rn.Grade != model.GradeAttributed {
		t.Fatalf("replacement pod not attributed: %+v", rn)
	}
	for _, p := range pods[:len(pods)-1] {
		if nodeByUID(g, p.Ref.UID) != nil {
			t.Errorf("untouched pod %s attached", p.Ref.Name)
		}
	}
}

func TestGraphIsDeterministicUnderReordering(t *testing.T) {
	c := synth.New(t0)
	c.Sources()
	w := c.Deployment("shop", "checkout", 3)
	c.Advance(time.Minute)
	restartScenario(c, w, synth.MCPOptions{})
	recs := c.Records()

	encode := func(recs []obs.Record) []byte {
		e, now := engineFor(t, recs)
		g := graphFor(t, e, now, byKind(model.ActionMCPToolCall))
		b, err := g.MarshalCanonical()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	want := encode(recs)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 5 {
		shuffled := slices.Clone(recs)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		// Duplicates must be idempotent.
		shuffled = append(shuffled, shuffled[:len(shuffled)/3]...)
		if got := encode(shuffled); !bytes.Equal(got, want) {
			t.Fatalf("shuffle %d produced a different graph", i)
		}
	}
}

func TestHumanScaleAttributesNewPodsOnly(t *testing.T) {
	c := synth.New(t0)
	c.Sources()
	w := c.Deployment("shop", "checkout", 2)
	existing := slices.Clone(w.Pods[w.Current])
	c.Advance(time.Minute)
	c.Audit(synth.Request{AuditID: "scale-1", Verb: "patch", Res: "deployments", Sub: "scale", NS: "shop", Name: "checkout", User: "kubernetes-admin", At: c.Now})
	w.Mutate()
	w.Scale(4)
	e, now := engineFor(t, c.Records())
	g, err := e.Graph(now, "k8s-scale-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range existing {
		if nodeByUID(g, p.Ref.UID) != nil {
			t.Errorf("pre-existing pod %s attached", p.Ref.Name)
		}
	}
	created := 0
	for _, n := range g.Nodes {
		if n.Object != nil && n.Object.Kind == "Pod" && n.Change != nil && n.Change.Type == "CREATED" {
			created++
		}
	}
	if created != 2 {
		t.Errorf("created pods = %d, want 2", created)
	}
	if n := nodeByUID(g, w.Current.Ref.UID); n == nil || n.Change == nil || n.Change.Type != "SCALED" {
		t.Errorf("ReplicaSet not SCALED: %+v", n)
	}
}

func TestControllerRequestsAreNotActions(t *testing.T) {
	c := synth.New(t0)
	c.Audit(synth.Request{AuditID: "rc-1", Verb: "create", Res: "pods", NS: "shop", Name: "x", User: "system:serviceaccount:kube-system:replicaset-controller", At: t0})
	c.Audit(synth.Request{AuditID: "hpa-1", Verb: "update", Res: "deployments", Sub: "scale", NS: "shop", Name: "x", User: "system:serviceaccount:kube-system:horizontal-pod-autoscaler", At: t0})
	c.Audit(synth.Request{AuditID: "ev-1", Verb: "create", Res: "events", NS: "shop", Name: "x", User: "kubernetes-admin", At: t0})
	e, now := engineFor(t, c.Records())
	var ids []string
	for _, a := range e.Actions(now, time.Time{}) {
		ids = append(ids, a.ID)
	}
	if !slices.Equal(ids, []string{"k8s-hpa-1"}) {
		t.Errorf("actions = %v, want only the HPA action", ids)
	}
}

func TestNoOpRequestNeverClaimsLaterEffects(t *testing.T) {
	// Regression: a kubectl apply that changes nothing, issued shortly
	// before an agent's restart, must not claim the restart's rollout.
	c := synth.New(t0)
	c.Sources()
	w := c.Deployment("shop", "checkout", 3)
	c.Advance(time.Minute)
	c.Audit(synth.Request{AuditID: "noop-apply", Verb: "patch", Res: "deployments", NS: "shop", Name: "checkout", User: "kubernetes-admin", At: c.Now})
	c.Advance(1500 * time.Millisecond)
	restartScenario(c, w, synth.MCPOptions{})
	e, now := engineFor(t, c.Records())
	g := graphFor(t, e, now, byKind(model.ActionMCPToolCall))
	if len(g.Ambiguities) != 0 {
		t.Fatalf("no-op request created ambiguities: %+v", g.Ambiguities)
	}
	if n := nodeByUID(g, w.Current.Ref.UID); n == nil || n.Grade != model.GradeAttributed {
		t.Fatalf("new ReplicaSet not attributed to the restart: %+v", n)
	}
	noop, err := e.Graph(now, "k8s-noop-apply")
	if err != nil {
		t.Fatal(err)
	}
	dep := nodeByUID(noop, w.Deploy.Ref.UID)
	if dep == nil || dep.Change.Type != "UNCHANGED" {
		t.Fatalf("no-op request should report UNCHANGED: %+v", dep)
	}
	for _, ed := range noop.Edges {
		if ed.Evidence == model.EvidenceStructural {
			t.Errorf("no-op request has structural edge to %s", ed.To)
		}
	}
}
