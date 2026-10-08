package experiments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/pkg/model"
)

const graphTimeout = 4 * time.Minute

// deploymentAction runs one action against a Deployment and evaluates it.
func deploymentAction(ctx context.Context, e *Env, r *Result, deploy string, act func() (*Outcome, error)) (*Outcome, error) {
	uid := e.Rec.UIDOf("Deployment", deploy)
	if uid == "" {
		return nil, fmt.Errorf("deployment %s not observed", deploy)
	}
	o, err := act()
	if err != nil {
		return o, err
	}
	if err := e.waitStable(ctx, 3*time.Minute, deploy); err != nil {
		return o, err
	}
	if err := settle(ctx); err != nil {
		return o, err
	}
	o.End = time.Now()
	err = e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error {
		workloadTruth(objs, o, uid, deploy)
		return nil
	}, o)
	if err == nil {
		standardChecks(r, o)
	}
	return o, err
}

func mcpOn(deploy, tool string, args map[string]any) func(context.Context, *Env, *Result) error {
	return func(ctx context.Context, e *Env, r *Result) error {
		_, err := deploymentAction(ctx, e, r, deploy, func() (*Outcome, error) {
			return e.mcp(ctx, "agent "+tool+" "+deploy, tool, args, false)
		})
		return err
	}
}

func args(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// control records a positive control: the injected fault must be visible in
// Prometheus, otherwise the experiment proves nothing.
func control(ctx context.Context, e *Env, r *Result, what, query string, from, to time.Time, threshold float64) {
	v, err := e.Lab.PromMax(ctx, query, from, to)
	r.check("positive control: "+what, err == nil && v > threshold, "max %.3f (threshold %.3f) %v", v, threshold, err)
}

func findAction(ctx context.Context, e *Env, since time.Time, timeout time.Duration, match func(correlate.ActionSummary) bool) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		q := url.Values{"since": {since.UTC().Format(time.RFC3339Nano)}}
		b, _, err := e.Lab.Get(ctx, "/api/v1/actions?"+q.Encode())
		if err == nil {
			var resp struct {
				Actions []correlate.ActionSummary `json:"actions"`
			}
			if json.Unmarshal(b, &resp) == nil {
				for _, a := range resp.Actions {
					if match(a) {
						return a.ID, nil
					}
				}
			}
		}
		if err := pause(ctx, 2*time.Second); err != nil {
			return "", err
		}
	}
	return "", errors.New("matching action not found")
}

const demoActor = "system:serviceaccount:effecttrace-demo:demo-actor"

// LiveScenarios returns the kind experiments in execution order.
func LiveScenarios() []Scenario {
	return []Scenario{
		{ID: "L01", Title: "MCP tool restarts a Deployment", Category: "live",
			Description: "The scripted agent calls restart_workload(shop/checkout). Expect a DIRECT edge to the Deployment confirmed by the audit event, the new ReplicaSet and Pods, and the old Pods terminating.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				o, err := deploymentAction(ctx, e, r, "checkout", func() (*Outcome, error) {
					return e.mcp(ctx, "agent restart_workload checkout", "restart_workload", args("namespace", "shop", "deployment", "checkout"), false)
				})
				if err != nil {
					return err
				}
				r.check("tool span links to the Kubernetes request", hasEdge(o.Graph, model.EvidenceTraceLink, "PATCH deployments"), "TRACE_LINK edge to the PATCH request")
				dep := coverage(o.Graph, "kubernetes-audit")
				r.check("request confirmed by audit event", dep.Available, "%s", dep.Detail)
				return nil
			}},
		{ID: "L02", Title: "MCP tool scales a Deployment up", Category: "live",
			Description: "scale_workload(shop/checkout, 5) through the scale subresource. Expect the existing ReplicaSet scaled and two new Pods; existing Pods untouched.",
			Run:         mcpOn("checkout", "scale_workload", args("namespace", "shop", "deployment", "checkout", "replicas", 5))},
		{ID: "L03", Title: "MCP tool scales a Deployment down", Category: "live",
			Description: "scale_workload(shop/checkout, 1). Expect the ReplicaSet scaled down and two Pods terminated.",
			Run:         mcpOn("checkout", "scale_workload", args("namespace", "shop", "deployment", "checkout", "replicas", 1))},
		{ID: "L04", Title: "MCP tool changes an image", Category: "live",
			Description: "update_image(shop/payments, app, demo:v2). Expect a rollout: new ReplicaSet, new Pods, old Pods terminated.",
			Run:         mcpOn("payments", "update_image", args("namespace", "shop", "deployment", "payments", "container", "app", "image", "localhost/effecttrace/demo:v2"))},
		{ID: "L05", Title: "MCP tool rolls an image back", Category: "live",
			Description: "After a human sets payments to demo:v2, the agent sets it back to demo:dev. The Deployment controller reuses the previous ReplicaSet; expect it attributed as SCALED, not CREATED.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				if _, err := e.Lab.KubectlQuiet(ctx, "-n", Namespace, "set", "image", "deployment/payments", "app=localhost/effecttrace/demo:v2"); err != nil {
					return err
				}
				if err := e.waitStable(ctx, 3*time.Minute, "payments"); err != nil {
					return err
				}
				if err := settle(ctx); err != nil {
					return err
				}
				prev := ""
				for uid, l := range e.Rec.Snapshot() {
					if l.Kind == "ReplicaSet" && strings.HasPrefix(l.Name, "payments-") && l.Deleted.IsZero() && len(l.Replicas) > 0 && l.Replicas[len(l.Replicas)-1].To == 0 {
						if l.Owner == e.Rec.UIDOf("Deployment", "payments") {
							prev = uid
						}
					}
				}
				o, err := deploymentAction(ctx, e, r, "payments", func() (*Outcome, error) {
					return e.mcp(ctx, "agent update_image payments back to dev", "update_image", args("namespace", "shop", "deployment", "payments", "container", "app", "image", "localhost/effecttrace/demo:dev"), false)
				})
				if err != nil {
					return err
				}
				reused := false
				for _, n := range o.Graph.Nodes {
					if n.Object != nil && n.Object.UID == prev && n.Change != nil && n.Change.Type == "SCALED" {
						reused = true
					}
				}
				r.check("previous ReplicaSet reused and attributed as SCALED", prev != "" && reused, "previous ReplicaSet %s", prev)
				return nil
			}},
		{ID: "L06", Title: "MCP tool triggers a failed rollout", Category: "live",
			Description: "update_image(shop/checkout) to a tag that does not exist. The rollout never becomes stable; expect the window to be capped, the unavailable Pod attributed and never ready, and its failure Events attached.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				uid := e.Rec.UIDOf("Deployment", "checkout")
				o, err := e.mcp(ctx, "agent update_image checkout to a missing tag", "update_image", args("namespace", "shop", "deployment", "checkout", "container", "app", "image", "localhost/effecttrace/demo:does-not-exist"), false)
				if err != nil {
					return err
				}
				if err := e.resolveMCP(ctx, o); err != nil {
					return err
				}
				// The lab collector caps reconciliation at 90s.
				if err := pause(ctx, 97*time.Second); err != nil {
					return err
				}
				o.End = time.Now()
				if err := e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error { workloadTruth(objs, o, uid, "checkout"); return nil }, o); err != nil {
					return err
				}
				standardChecks(r, o)
				capped := false
				for _, w := range o.Graph.Windows {
					if strings.Contains(w.Reason, "capped") {
						capped = true
					}
				}
				r.check("window capped because the rollout never stabilized", capped, "reconcile window reason")
				notReady, failEvents := 0, 0
				for _, n := range o.Graph.Nodes {
					if n.Object != nil && n.Object.Kind == "Pod" && n.Change != nil && n.Change.Type == "CREATED" && n.Change.ReadyAt.IsZero() {
						notReady++
					}
					if n.Event != nil && (n.Event.Reason == "Failed" || n.Event.Reason == "BackOff" || n.Event.Reason == "ErrImageNeverPull") {
						failEvents++
					}
				}
				r.check("new Pod attributed and never ready", notReady > 0, "%d unready created Pod(s)", notReady)
				r.check("image failure Events attached", failEvents > 0, "%d failure Event(s)", failEvents)
				return nil
			}},
		{ID: "L07", Title: "MCP tool deletes a Pod", Category: "live",
			Description: "delete_pod on one checkout Pod. Expect DIRECT on the deleted Pod and the ReplicaSet's replacement Pod attributed; sibling Pods untouched.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				pods, err := e.Lab.Client.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=checkout"})
				if err != nil || len(pods.Items) == 0 {
					return fmt.Errorf("no checkout pods: %w", err)
				}
				victim := pods.Items[0]
				owner := metav1.GetControllerOf(&victim)
				o, err := e.mcp(ctx, "agent delete_pod "+victim.Name, "delete_pod", args("namespace", "shop", "pod", victim.Name), false)
				if err != nil {
					return err
				}
				if err := e.waitStable(ctx, 2*time.Minute, "checkout"); err != nil {
					return err
				}
				if err := settle(ctx); err != nil {
					return err
				}
				o.End = time.Now()
				if err := e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error {
					o.Truth = Truth{Direct: []string{string(victim.UID)}, Structural: ReplacementTruth(objs, string(owner.UID), string(victim.UID), o.Started, o.End), Workloads: []string{"checkout"}}
					return nil
				}, o); err != nil {
					return err
				}
				standardChecks(r, o)
				return nil
			}},
		{ID: "L08", Title: "MCP tool modifies a ConfigMap", Category: "live",
			Description: "set_config on shop/shop-config with synthetic data. Kubernetes has no ownership from a ConfigMap to Pods, so expect a DIRECT edge only and no structural fan-out. EffectTrace never watches ConfigMaps; the UID comes from the instrumented client response.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				uid := e.Rec.UIDOf("ConfigMap", "shop-config")
				o, err := e.mcp(ctx, "agent set_config shop-config", "set_config", args("namespace", "shop", "configmap", "shop-config", "key", "discount-banner", "value", fmt.Sprintf("winter-%d", time.Now().Unix()%1000)), false)
				if err != nil {
					return err
				}
				if err := settle(ctx); err != nil {
					return err
				}
				o.End = time.Now()
				if err := e.finish(ctx, r, graphTimeout, func(map[string]Life) error { o.Truth = Truth{Direct: []string{uid}}; return nil }, o); err != nil {
					return err
				}
				standardChecks(r, o)
				r.check("no structural fan-out claimed", o.Eval.EdgeCounts["STRUCTURAL"] == 0, "%d STRUCTURAL edges", o.Eval.EdgeCounts["STRUCTURAL"])
				return nil
			}},
		{ID: "L09", Title: "MCP tool changes a Service selector", Category: "live",
			Description: "update_service_selector adds a label the inventory Pods do not have, removing all endpoints. This has real traffic effects, but Service-to-EndpointSlice is not a relationship EffectTrace follows in v0.1: expect DIRECT only, no structural claims and no attached telemetry.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				uid := e.Rec.UIDOf("Service", "inventory")
				o, err := e.mcp(ctx, "agent update_service_selector inventory", "update_service_selector", args("namespace", "shop", "service", "inventory", "key", "track", "value", "canary"), false)
				if err != nil {
					return err
				}
				if err := pause(ctx, 10*time.Second); err != nil {
					return err
				}
				o.End = time.Now()
				ferr := e.finish(ctx, r, graphTimeout, func(map[string]Life) error { o.Truth = Truth{Direct: []string{uid}}; return nil }, o)
				if _, err := e.Lab.KubectlQuiet(ctx, "-n", Namespace, "patch", "service", "inventory", "--type=json", "-p", `[{"op":"remove","path":"/spec/selector/track"}]`); err != nil {
					return err
				}
				if ferr != nil {
					return ferr
				}
				standardChecks(r, o)
				r.check("no telemetry attached to a non-workload object", o.Eval.EdgeCounts["TEMPORAL_CORRELATION"] == 0, "%d TEMPORAL edges", o.Eval.EdgeCounts["TEMPORAL_CORRELATION"])
				r.note("Traffic to inventory failed while the selector matched no Pods; EffectTrace v0.1 does not follow Service selectors, so those effects appear at most as exclusions.")
				return nil
			}},
		{ID: "L10", Title: "MCP tool changes resource limits", Category: "live",
			Description: "set_resources(shop/inventory) changes the Pod template. Expect a rollout attributed to the tool call.",
			Run:         mcpOn("inventory", "set_resources", args("namespace", "shop", "deployment", "inventory", "container", "app", "cpu_limit", "250m", "memory_limit", "80Mi"))},
		{ID: "L11", Title: "Human kubectl scale", Category: "live",
			Description: "kubectl scale deployment/payments --replicas=3. No trace exists; the action comes from the audit log and the UID is resolved by name at request time.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				o, err := deploymentAction(ctx, e, r, "payments", func() (*Outcome, error) {
					return e.kubectl(ctx, "kubectl scale payments", "-n", Namespace, "scale", "deployment/payments", "--replicas=3")
				})
				if err != nil {
					return err
				}
				nameResolved := false
				for _, ed := range o.Graph.Edges {
					if ed.Rule == correlate.RuleNameAtRequestTime {
						nameResolved = true
					}
				}
				r.check("UID resolved by name at request time", nameResolved, "rule %s", correlate.RuleNameAtRequestTime)
				return nil
			}},
		{ID: "L12", Title: "Human kubectl rollout restart", Category: "live",
			Description: "kubectl rollout restart deployment/frontend, attributed from the audit log alone.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				_, err := deploymentAction(ctx, e, r, "frontend", func() (*Outcome, error) {
					return e.kubectl(ctx, "kubectl rollout restart frontend", "-n", Namespace, "rollout", "restart", "deployment/frontend")
				})
				return err
			}},
		{ID: "L13", Title: "Human kubectl delete pod", Category: "live",
			Description: "kubectl delete pod on one payments Pod; expect the replacement Pod attributed.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				pods, err := e.Lab.Client.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=payments"})
				if err != nil || len(pods.Items) == 0 {
					return fmt.Errorf("no payments pods: %w", err)
				}
				victim := pods.Items[0]
				owner := metav1.GetControllerOf(&victim)
				o, err := e.kubectl(ctx, "kubectl delete pod "+victim.Name, "-n", Namespace, "delete", "pod", victim.Name, "--wait=false")
				if err != nil {
					return err
				}
				if err := e.waitStable(ctx, 2*time.Minute, "payments"); err != nil {
					return err
				}
				if err := settle(ctx); err != nil {
					return err
				}
				o.End = time.Now()
				if err := e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error {
					o.Truth = Truth{Direct: []string{string(victim.UID)}, Structural: ReplacementTruth(objs, string(owner.UID), string(victim.UID), o.Started, o.End), Workloads: []string{"payments"}}
					return nil
				}, o); err != nil {
					return err
				}
				standardChecks(r, o)
				return nil
			}},
		{ID: "L14", Title: "Two agents change two workloads concurrently", Category: "live",
			Description: "Agent A restarts checkout while agent B scales payments 2 -> 4 at the same moment. Each graph must contain only its own workload's effects.",
			Run: concurrent(
				act{"agent A restart_workload checkout", "checkout", "mcp", "restart_workload", args("namespace", "shop", "deployment", "checkout"), nil},
				act{"agent B scale_workload payments", "payments", "mcp", "scale_workload", args("namespace", "shop", "deployment", "payments", "replicas", 4), nil},
			)},
		{ID: "L15", Title: "Agent and human change two workloads concurrently", Category: "live",
			Description: "The agent restarts checkout while a human scales inventory with kubectl at the same moment.",
			Run: concurrent(
				act{"agent restart_workload checkout", "checkout", "mcp", "restart_workload", args("namespace", "shop", "deployment", "checkout"), nil},
				act{"kubectl scale inventory", "inventory", "kubectl", "", nil, []string{"-n", Namespace, "scale", "deployment/inventory", "--replicas=3"}},
			)},
		{ID: "L16", Title: "Agent and human change the same workload", Category: "live",
			Description: "The agent restarts checkout; 1.5 s later a human scales checkout 3 -> 4 while the rollout is in progress. Changes after the second request are genuinely ambiguous: expect them reported as ambiguities and claimed by neither action.",
			Run:         sameWorkload("kubectl"),
		},
		{ID: "L17", Title: "Two rapid restarts fan out over three ReplicaSets", Category: "live",
			Description: "The agent restarts checkout twice, 1.5 s apart. Three ReplicaSets take part; changes after the second restart are ambiguous between the two actions.",
			Run:         sameWorkload("mcp"),
		},
		{ID: "L18", Title: "Unrelated latency spike in another service", Category: "live", Quiet: true,
			Description: "The agent scales inventory 2 -> 3 while a runtime fault (no Kubernetes change) adds 300 ms latency to payments. The payments signal must not be attached to the inventory action; it should appear as an exclusion.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				if err := e.Lab.Fault(ctx, "payments", 300, 0, 45*time.Second); err != nil {
					return err
				}
				o, err := deploymentAction(ctx, e, r, "inventory", func() (*Outcome, error) {
					return e.mcp(ctx, "agent scale_workload inventory", "scale_workload", args("namespace", "shop", "deployment", "inventory", "replicas", 3), false)
				})
				if err != nil {
					return err
				}
				control(ctx, e, r, "payments p99 rose during the window", P99("payments"), o.Started, o.End, 0.2)
				excluded := false
				for _, x := range o.Graph.Exclusions {
					if x.Kind == "METRIC" && strings.Contains(x.Label, "payments") {
						excluded = true
					}
				}
				r.check("payments latency reported as an exclusion", excluded, "exclusion list")
				return nil
			}},
		{ID: "L19", Title: "Unrelated error spike in another service", Category: "live", Quiet: true,
			Description: "The agent scales payments 2 -> 3 while a runtime fault makes 30% of inventory requests fail. inventory, checkout and frontend error signals must not attach to the payments action.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				if err := e.Lab.Fault(ctx, "inventory", 0, 0.3, 45*time.Second); err != nil {
					return err
				}
				o, err := deploymentAction(ctx, e, r, "payments", func() (*Outcome, error) {
					return e.mcp(ctx, "agent scale_workload payments", "scale_workload", args("namespace", "shop", "deployment", "payments", "replicas", 3), false)
				})
				if err != nil {
					return err
				}
				control(ctx, e, r, "inventory error ratio rose during the window", ErrorRatio("inventory"), o.Started, o.End, 0.1)
				excluded := false
				for _, x := range o.Graph.Exclusions {
					if x.Kind == "METRIC" && strings.Contains(x.Label, "http_error_ratio for inventory") {
						excluded = true
					}
				}
				r.check("inventory errors reported as an exclusion", excluded, "exclusion list")
				return nil
			}},
		{ID: "L20", Title: "Unrelated fault inside the window of the same workload", Category: "live", Quiet: true,
			Description: "The agent scales checkout 3 -> 4; a runtime fault (not caused by the action, by construction) adds 250 ms latency to checkout inside the window. EffectTrace must attach it only as TEMPORAL_CORRELATION and state that correlation is not causation. This experiment shows why the evidence class matters.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				o, err := deploymentAction(ctx, e, r, "checkout", func() (*Outcome, error) {
					o, err := e.mcp(ctx, "agent scale_workload checkout", "scale_workload", args("namespace", "shop", "deployment", "checkout", "replicas", 4), false)
					if err == nil {
						err = e.Lab.Fault(ctx, "checkout", 250, 0, 30*time.Second)
					}
					return o, err
				})
				if err != nil {
					return err
				}
				control(ctx, e, r, "checkout p99 rose during the window", P99("checkout"), o.Started, o.End, 0.2)
				attached := hasEdge(o.Graph, model.EvidenceTemporalCorrelation, "http_p99_latency for checkout")
				onlyTemporal := true
				for _, ed := range o.Graph.Edges {
					if strings.HasPrefix(ed.To, "metric:") && ed.Evidence != model.EvidenceTemporalCorrelation {
						onlyTemporal = false
					}
				}
				noted := false
				for _, n := range o.Graph.Notes {
					if strings.Contains(n, "does not prove causation") {
						noted = true
					}
				}
				r.check("latency change attached as TEMPORAL_CORRELATION", attached, "checkout p99 edge")
				r.check("telemetry never carries stronger evidence", onlyTemporal, "metric edges")
				r.check("graph states that correlation is not causation", noted, "graph notes")
				r.note("Ground truth: the latency was injected by the harness and is causally unrelated to the scale action. The TEMPORAL_CORRELATION label is the correct, honest description of what EffectTrace can know.")
				return nil
			}},
		{ID: "L21", Title: "Metric change after the window", Category: "live", Quiet: true,
			Description: "After the inventory action's graph is complete, a fault adds 400 ms latency to inventory. The completed graph must not change.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				o, err := deploymentAction(ctx, e, r, "inventory", func() (*Outcome, error) {
					return e.mcp(ctx, "agent scale_workload inventory", "scale_workload", args("namespace", "shop", "deployment", "inventory", "replicas", 3), false)
				})
				if err != nil {
					return err
				}
				before, err := o.Graph.MarshalCanonical()
				if err != nil {
					return err
				}
				if err := e.Lab.Fault(ctx, "inventory", 400, 0, 30*time.Second); err != nil {
					return err
				}
				faultAt := time.Now()
				if err := pause(ctx, 25*time.Second); err != nil {
					return err
				}
				control(ctx, e, r, "inventory p99 rose after the window", P99("inventory"), faultAt, time.Now(), 0.3)
				b, _, err := e.Lab.Get(ctx, "/api/v1/effects/"+o.ActionID)
				if err != nil {
					return err
				}
				g, err := model.UnmarshalGraph(b)
				if err != nil {
					return err
				}
				after, _ := g.MarshalCanonical()
				r.check("completed graph unchanged by a later metric change", bytes.Equal(before, after), "canonical JSON compared")
				return nil
			}},
		{ID: "L22", Title: "Same name deleted and recreated with a new UID", Category: "live",
			Description: "A human creates Deployment ephemeral, deletes it, and recreates it with the same name; then the agent restarts it. The restart graph must contain only the new UID, and the delete graph only the old objects.",
			Run:         sameNameRecreate,
		},
		{ID: "L23", Title: "Trace context missing between agent and tool server", Category: "live",
			Description: "The agent does not propagate traceparent. The tool span becomes a root span, but the Kubernetes client span still descends from it, so DIRECT evidence is unaffected; only the agent identity is lost.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				o, err := deploymentAction(ctx, e, r, "frontend", func() (*Outcome, error) {
					o, err := e.mcp(ctx, "agent (no trace context) scale_workload frontend", "scale_workload", args("namespace", "shop", "deployment", "frontend", "replicas", 3), true)
					if err != nil {
						return o, err
					}
					// Without propagated context the tool span starts a new
					// trace, so it cannot be found by the agent's trace ID.
					id, err := findAction(ctx, e, o.Started.Add(-5*time.Second), time.Minute, func(a correlate.ActionSummary) bool {
						return a.Kind == model.ActionMCPToolCall && a.Name == "tools/call scale_workload" && a.TraceID != o.TraceID
					})
					o.ActionID = id
					return o, err
				})
				if err != nil {
					return err
				}
				r.check("calling agent unknown without context", o.Graph.Action.Actor == "", "actor %q", o.Graph.Action.Actor)
				return nil
			}},
		{ID: "L24", Title: "Trace context missing between tool and Kubernetes client", Category: "live",
			Description: "The tool server runs with an uninstrumented Kubernetes client. The tool call must not receive DIRECT evidence; it may only reach the request through TEMPORAL_CORRELATION, while the request itself remains an attributed audit-log action.",
			Run:         uninstrumentedClient,
		},
		{ID: "L25", Title: "EffectTrace collector restarts during a rollout", Category: "live",
			Description: "The collector Pod is deleted 1 s after the agent restarts checkout. In-memory state and spans are lost; after restart the collector re-reads the audit log and relists objects. Expect degraded but honest results: no false attachments and partial watch coverage reported.",
			Run:         collectorRestart,
		},
		{ID: "L26", Title: "OpenTelemetry Collector restarts", Category: "live",
			Description: "The OTel Collector Pod is deleted right before the agent scales frontend. Spans may be delayed or lost; EffectTrace must still attribute the change exactly once, either to the tool call or to the audit-log action.",
			Run:         otelRestart,
		},
		{ID: "L27", Title: "Audit log unavailable", Category: "live",
			Description: "The collector is reconfigured to a missing audit log path. DIRECT evidence then rests on the instrumented client response alone, and audit coverage is reported as missing.",
			Run:         missingAudit,
		},
		{ID: "L28", Title: "Metrics source unavailable", Category: "live",
			Description: "Prometheus is scaled to zero before the agent restarts payments. Structural attribution must be unaffected, no temporal edges may appear and Prometheus coverage must be reported as unavailable.",
			Run:         missingMetrics,
		},
		{ID: "L29", Title: "kube-controller-manager restarts during a rollout", Category: "live",
			Description: "The agent restarts frontend and the kube-controller-manager container is stopped immediately afterwards; kubelet restarts it and reconciliation resumes. Attribution must remain correct.",
			Run:         controllerRestart,
		},
		{ID: "L30", Title: "Hostile and forged telemetry", Category: "live",
			Description: "Spans with terminal escape sequences, zero trace IDs, oversized attributes, HTML tool names and a forged client span that names a real audit ID with the wrong verb are sent through the OTel Collector. EffectTrace must reject invalid spans, keep existing graphs unchanged, escape HTML in API output and refuse the mismatched audit confirmation.",
			Run:         hostileTelemetry,
		},
		{ID: "L31", Title: "Burst of four concurrent tool calls", Category: "live",
			Description: "Four agents act on four different workloads at once.",
			Run: concurrent(
				act{"agent restart_workload frontend", "frontend", "mcp", "restart_workload", args("namespace", "shop", "deployment", "frontend"), nil},
				act{"agent scale_workload checkout", "checkout", "mcp", "scale_workload", args("namespace", "shop", "deployment", "checkout", "replicas", 4), nil},
				act{"agent restart_workload payments", "payments", "mcp", "restart_workload", args("namespace", "shop", "deployment", "payments"), nil},
				act{"agent scale_workload inventory", "inventory", "mcp", "scale_workload", args("namespace", "shop", "deployment", "inventory", "replicas", 3), nil},
			)},
		{ID: "L32", Title: "Burst of eight concurrent tool calls, two per workload", Category: "live",
			Description: "Eight agents act at once, two on each workload. Same-workload pairs are inherently ambiguous; cross-workload separation must be perfect and ambiguous changes must be claimed by no action.",
			Run:         burstPairs,
		},
		{ID: "L33", Title: "Scale to zero, then back up", Category: "live",
			Description: "The agent scales inventory to 0 and, once stable, back to 2. Each action must own only its own Pods.",
			Run: func(ctx context.Context, e *Env, r *Result) error {
				if _, err := deploymentAction(ctx, e, r, "inventory", func() (*Outcome, error) {
					return e.mcp(ctx, "agent scale inventory to 0", "scale_workload", args("namespace", "shop", "deployment", "inventory", "replicas", 0), false)
				}); err != nil {
					return err
				}
				_, err := deploymentAction(ctx, e, r, "inventory", func() (*Outcome, error) {
					return e.mcp(ctx, "agent scale inventory to 2", "scale_workload", args("namespace", "shop", "deployment", "inventory", "replicas", 2), false)
				})
				return err
			}},
	}
}

type act struct {
	label, deploy, via, tool string
	args                     map[string]any
	kubectl                  []string
}

func (e *Env) run(ctx context.Context, a act) (*Outcome, error) {
	if a.via == "mcp" {
		return e.mcp(ctx, a.label, a.tool, a.args, false)
	}
	return e.kubectl(ctx, a.label, a.kubectl...)
}

// concurrent runs actions on different workloads at the same moment.
func concurrent(acts ...act) func(context.Context, *Env, *Result) error {
	return func(ctx context.Context, e *Env, r *Result) error {
		uids := map[string]string{}
		for _, a := range acts {
			uids[a.deploy] = e.Rec.UIDOf("Deployment", a.deploy)
		}
		outs := make([]*Outcome, len(acts))
		errs := make([]error, len(acts))
		var wg sync.WaitGroup
		for i, a := range acts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outs[i], errs[i] = e.run(ctx, a)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return err
		}
		var names []string
		for _, a := range acts {
			names = append(names, a.deploy)
		}
		if err := e.waitStable(ctx, 3*time.Minute, names...); err != nil {
			return err
		}
		if err := settle(ctx); err != nil {
			return err
		}
		end := time.Now()
		for _, o := range outs {
			o.End = end
		}
		err := e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error {
			for i, a := range acts {
				workloadTruth(objs, outs[i], uids[a.deploy], a.deploy)
			}
			return nil
		}, outs...)
		if err != nil {
			return err
		}
		for _, o := range outs {
			standardChecks(r, o)
		}
		separation(r)
		return nil
	}
}

// sameWorkload runs two actions on checkout 1.5 s apart.
func sameWorkload(second string) func(context.Context, *Env, *Result) error {
	return func(ctx context.Context, e *Env, r *Result) error {
		uid := e.Rec.UIDOf("Deployment", "checkout")
		a, err := e.mcp(ctx, "agent restart_workload checkout", "restart_workload", args("namespace", "shop", "deployment", "checkout"), false)
		if err != nil {
			return err
		}
		if err := pause(ctx, 1500*time.Millisecond); err != nil {
			return err
		}
		var b *Outcome
		if second == "kubectl" {
			b, err = e.kubectl(ctx, "kubectl scale checkout to 4", "-n", Namespace, "scale", "deployment/checkout", "--replicas=4")
		} else {
			b, err = e.mcp(ctx, "agent restart_workload checkout again", "restart_workload", args("namespace", "shop", "deployment", "checkout"), false)
		}
		if err != nil {
			return err
		}
		if err := e.waitStable(ctx, 3*time.Minute, "checkout"); err != nil {
			return err
		}
		if err := settle(ctx); err != nil {
			return err
		}
		end := time.Now()
		a.End, b.End = end, end
		var lateDeleted []string
		err = e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error {
			before := DescendantTruth(objs, uid, a.Started, b.Started)
			inflight := DescendantTruth(objs, uid, b.Started, b.Returned)
			after := DescendantTruth(objs, uid, b.Returned, end)
			var ambiguous, uncertain []string
			for _, u := range after {
				if !slices.Contains(before, u) && !slices.Contains(inflight, u) {
					ambiguous = append(ambiguous, u)
				}
				if l := objs[u]; slices.Contains(before, u) && !l.Deleted.IsZero() && !l.Deleted.Before(b.Returned) {
					lateDeleted = append(lateDeleted, u)
				}
			}
			for _, u := range inflight {
				if !slices.Contains(before, u) {
					uncertain = append(uncertain, u)
				}
			}
			a.Truth = Truth{Direct: []string{uid}, Structural: before, Workloads: []string{"checkout"}, Ambiguous: ambiguous, Uncertain: uncertain}
			b.Truth = Truth{Direct: []string{uid}, Workloads: []string{"checkout"}, Ambiguous: ambiguous, Uncertain: uncertain}
			return nil
		}, a, b)
		if err != nil {
			return err
		}
		for _, o := range []*Outcome{a, b} {
			r.check(o.Label+": no false attachments", len(o.Eval.FalseAttachments) == 0, "%v", o.Eval.FalseAttachments)
			r.check(o.Label+": no attributed claim on an ambiguous change", len(o.Eval.AmbiguousClaimed) == 0, "%v", o.Eval.AmbiguousClaimed)
			r.check(o.Label+": direct precision and recall", o.Eval.Direct.FP == 0 && o.Eval.Direct.FN == 0, "%+v", o.Eval.Direct)
			r.check(o.Label+": structural precision", o.Eval.Structural.FP == 0, "%+v", o.Eval.Structural)
		}
		r.check("ambiguities reported", a.Eval.Ambiguities > 0 && b.Eval.Ambiguities > 0, "first=%d second=%d", a.Eval.Ambiguities, b.Eval.Ambiguities)
		claimedLate := 0
		for _, n := range a.Graph.Nodes {
			if n.Object != nil && slices.Contains(lateDeleted, n.Object.UID) && n.Change != nil && !n.Change.DeletedAt.IsZero() {
				claimedLate++
			}
		}
		r.check("deletions after the second request not claimed by the first action", claimedLate == 0, "%d of %d", claimedLate, len(lateDeleted))
		r.note("Structural recall for the first action counts only changes before the second request; later changes are expected to be ambiguous.")
		r.Concurrent = true
		sep := len(a.Eval.AmbiguousClaimed) == 0 && len(b.Eval.AmbiguousClaimed) == 0 && len(a.Eval.FalseAttachments) == 0 && len(b.Eval.FalseAttachments) == 0
		r.Separated = &sep
		return nil
	}
}

func sameNameRecreate(ctx context.Context, e *Env, r *Result) error {
	manifest := e.Lab.Repo + "/test/experiments/testdata/ephemeral.yaml"
	if _, err := e.Lab.KubectlQuiet(ctx, "apply", "-f", manifest); err != nil {
		return err
	}
	if err := e.waitStable(ctx, 2*time.Minute, "ephemeral"); err != nil {
		return err
	}
	if err := settle(ctx); err != nil {
		return err
	}
	oldUID := e.Rec.UIDOf("Deployment", "ephemeral")
	del, err := e.kubectl(ctx, "kubectl delete deployment ephemeral", "-n", Namespace, "delete", "deployment", "ephemeral", "--wait=true", "--cascade=foreground")
	if err != nil {
		return err
	}
	// Wait until every object of the old Deployment is gone.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		pods, _ := e.Lab.Client.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=ephemeral"})
		if pods != nil && len(pods.Items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("old ephemeral pods not deleted")
		}
		if err := pause(ctx, time.Second); err != nil {
			return err
		}
	}
	if err := settle(ctx); err != nil {
		return err
	}
	del.End = time.Now()
	if _, err := e.Lab.KubectlQuiet(ctx, "apply", "-f", manifest); err != nil {
		return err
	}
	if err := e.waitStable(ctx, 2*time.Minute, "ephemeral"); err != nil {
		return err
	}
	if err := settle(ctx); err != nil {
		return err
	}
	newUID := e.Rec.UIDOf("Deployment", "ephemeral")
	if newUID == "" || newUID == oldUID {
		return fmt.Errorf("recreated deployment has no new UID (%s)", newUID)
	}
	o, err := deploymentAction(ctx, e, r, "ephemeral", func() (*Outcome, error) {
		return e.mcp(ctx, "agent restart_workload ephemeral (recreated)", "restart_workload", args("namespace", "shop", "deployment", "ephemeral"), false)
	})
	if err != nil {
		return err
	}
	if err := e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error {
		del.Truth = Truth{Direct: []string{oldUID}, Structural: DescendantTruth(objs, oldUID, del.Started, del.End)}
		return nil
	}, del); err != nil {
		return err
	}
	standardChecks(r, del)
	containsOld := false
	for _, n := range o.Graph.Nodes {
		if n.Object != nil && n.Object.UID == oldUID {
			containsOld = true
		}
	}
	r.check("restart graph does not contain the deleted UID", !containsOld, "old UID %s", oldUID)
	_, err = e.Lab.KubectlQuiet(ctx, "-n", Namespace, "delete", "deployment", "ephemeral", "--wait=true")
	return err
}

func setCollectorArgs(ctx context.Context, e *Env, replace map[string]string) error {
	d, err := e.Lab.Client.AppsV1().Deployments("effecttrace-system").Get(ctx, "effecttrace-collector", metav1.GetOptions{})
	if err != nil {
		return err
	}
	argsList := d.Spec.Template.Spec.Containers[0].Args
	for i, a := range argsList {
		for prefix, v := range replace {
			if strings.HasPrefix(a, prefix+"=") {
				argsList[i] = prefix + "=" + v
			}
		}
	}
	patch, _ := json.Marshal([]map[string]any{{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": argsList}})
	if _, err := e.Lab.KubectlQuiet(ctx, "-n", "effecttrace-system", "patch", "deployment", "effecttrace-collector", "--type=json", "-p", string(patch)); err != nil {
		return err
	}
	return waitCollector(ctx, e)
}

func waitCollector(ctx context.Context, e *Env) error {
	if _, err := e.Lab.KubectlQuiet(ctx, "-n", "effecttrace-system", "rollout", "status", "deployment/effecttrace-collector", "--timeout=180s"); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if _, _, err := e.Lab.Get(ctx, "/readyz"); err == nil {
			return nil
		}
		if err := pause(ctx, time.Second); err != nil {
			return err
		}
	}
	return errors.New("collector not ready")
}

func uninstrumentedClient(ctx context.Context, e *Env, r *Result) error {
	setEnv := func(v string) error {
		if _, err := e.Lab.KubectlQuiet(ctx, "-n", "effecttrace-demo", "set", "env", "deployment/mcp-tools", "UNINSTRUMENTED_KUBERNETES_CLIENT="+v); err != nil {
			return err
		}
		_, err := e.Lab.KubectlQuiet(ctx, "-n", "effecttrace-demo", "rollout", "status", "deployment/mcp-tools", "--timeout=120s")
		return err
	}
	if err := setEnv("true"); err != nil {
		return err
	}
	defer func() { _ = setEnv("false") }()
	if err := settle(ctx); err != nil {
		return err
	}
	uid := e.Rec.UIDOf("Deployment", "payments")
	m, err := e.mcp(ctx, "agent scale_workload payments (uninstrumented client)", "scale_workload", args("namespace", "shop", "deployment", "payments", "replicas", 3), false)
	if err != nil {
		return err
	}
	apiID, err := findAction(ctx, e, m.Started.Add(-5*time.Second), time.Minute, func(a correlate.ActionSummary) bool {
		return a.Kind == model.ActionKubernetesAPICall && a.Actor == demoActor && strings.Contains(a.Name, "deployments/scale shop/payments")
	})
	if err != nil {
		return err
	}
	api := &Outcome{Label: "audit-log action of the uninstrumented request", Kind: string(model.ActionKubernetesAPICall), ActionID: apiID, Started: m.Started}
	if err := e.waitStable(ctx, 3*time.Minute, "payments"); err != nil {
		return err
	}
	if err := settle(ctx); err != nil {
		return err
	}
	m.End, api.End = time.Now(), time.Now()
	err = e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error {
		workloadTruth(objs, api, uid, "payments")
		// The tool call has no attributable truth: its link is temporal only.
		m.Truth = Truth{Workloads: []string{"payments"}}
		return nil
	}, m, api)
	if err != nil {
		return err
	}
	standardChecks(r, api)
	r.check("tool call carries no attributed DIRECT claim", m.Eval.Direct.TP+m.Eval.Direct.FP == 0, "%+v", m.Eval.Direct)
	r.check("tool call reaches the request only through TEMPORAL_CORRELATION", hasEdge(m.Graph, model.EvidenceTemporalCorrelation, "deployments/scale"), "temporal edge")
	r.check("effects visible from the tool call are graded CORRELATED", m.Eval.Correlated > 0, "%d correlated objects", m.Eval.Correlated)
	return nil
}

func collectorRestart(ctx context.Context, e *Env, r *Result) error {
	uid := e.Rec.UIDOf("Deployment", "checkout")
	m, err := e.mcp(ctx, "agent restart_workload checkout", "restart_workload", args("namespace", "shop", "deployment", "checkout"), false)
	if err != nil {
		return err
	}
	if err := pause(ctx, time.Second); err != nil {
		return err
	}
	if _, err := e.Lab.KubectlQuiet(ctx, "-n", "effecttrace-system", "delete", "pod", "-l", "app.kubernetes.io/name=effecttrace-collector", "--wait=false"); err != nil {
		return err
	}
	if err := pause(ctx, 3*time.Second); err != nil {
		return err
	}
	if err := waitCollector(ctx, e); err != nil {
		return err
	}
	if err := e.waitStable(ctx, 3*time.Minute, "checkout"); err != nil {
		return err
	}
	if err := settle(ctx); err != nil {
		return err
	}
	// The tool span may have been lost; the audit log is re-read.
	id := ""
	if aid, err := e.Lab.ActionForTrace(ctx, m.TraceID, 20*time.Second); err == nil {
		id = aid
		r.note("The tool span survived the restart.")
	} else {
		id, err = findAction(ctx, e, m.Started.Add(-5*time.Second), time.Minute, func(a correlate.ActionSummary) bool {
			return a.Kind == model.ActionKubernetesAPICall && a.Actor == demoActor && strings.Contains(a.Name, "deployments shop/checkout")
		})
		if err != nil {
			return err
		}
		r.note("The tool span was lost with the collector's memory; the request was recovered from the audit log as an API-call action.")
	}
	o := &Outcome{Label: "action after collector restart", ActionID: id, Started: m.Started, End: time.Now(), Kind: "recovered"}
	if err := e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error { workloadTruth(objs, o, uid, "checkout"); return nil }, o); err != nil {
		return err
	}
	r.check("no false attachments after restart", len(o.Eval.FalseAttachments) == 0, "%v", o.Eval.FalseAttachments)
	r.check("structural precision after restart", o.Eval.Structural.FP == 0, "%+v", o.Eval.Structural)
	r.check("direct claim recovered", o.Eval.Direct.TP == 1 && o.Eval.Direct.FP == 0, "%+v", o.Eval.Direct)
	w := coverage(o.Graph, "kubernetes-watch")
	r.check("partial watch coverage reported", !w.Available, "%s", w.Detail)
	r.note("Structural recall after restart: %d of %d expected changes (changes during the downtime were not observed).", o.Eval.Structural.TP, o.Eval.Structural.TP+o.Eval.Structural.FN)
	return nil
}

func otelRestart(ctx context.Context, e *Env, r *Result) error {
	uid := e.Rec.UIDOf("Deployment", "frontend")
	if _, err := e.Lab.KubectlQuiet(ctx, "-n", "observability", "delete", "pod", "-l", "app=otel-collector", "--wait=false"); err != nil {
		return err
	}
	m, callErr := e.mcp(ctx, "agent scale_workload frontend", "scale_workload", args("namespace", "shop", "deployment", "frontend", "replicas", 3), false)
	if callErr != nil {
		return callErr
	}
	if _, err := e.Lab.KubectlQuiet(ctx, "-n", "observability", "rollout", "status", "deployment/otel-collector", "--timeout=120s"); err != nil {
		return err
	}
	if err := e.waitStable(ctx, 3*time.Minute, "frontend"); err != nil {
		return err
	}
	if err := settle(ctx); err != nil {
		return err
	}
	var outs []*Outcome
	if id, err := e.Lab.ActionForTrace(ctx, m.TraceID, 30*time.Second); err == nil {
		m.ActionID = id
		outs = append(outs, m)
		r.note("Spans were delivered after the OTel Collector restarted; the change is attributed to the tool call.")
	} else {
		id, err := findAction(ctx, e, m.Started.Add(-5*time.Second), time.Minute, func(a correlate.ActionSummary) bool {
			return a.Kind == model.ActionKubernetesAPICall && a.Actor == demoActor && strings.Contains(a.Name, "frontend")
		})
		if err != nil {
			return err
		}
		outs = append(outs, &Outcome{Label: "audit-log action (spans lost)", Kind: string(model.ActionKubernetesAPICall), ActionID: id, Started: m.Started})
		r.note("Spans were lost while the OTel Collector restarted; the change is attributed to the audit-log action.")
	}
	outs[0].End = time.Now()
	if err := e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error { workloadTruth(objs, outs[0], uid, "frontend"); return nil }, outs...); err != nil {
		return err
	}
	standardChecks(r, outs[0])
	return nil
}

func missingAudit(ctx context.Context, e *Env, r *Result) error {
	if err := setCollectorArgs(ctx, e, map[string]string{"--audit-log": "/var/log/kubernetes/audit/does-not-exist.log"}); err != nil {
		return err
	}
	defer func() {
		_ = setCollectorArgs(context.Background(), e, map[string]string{"--audit-log": "/var/log/kubernetes/audit/audit.log"})
	}()
	if err := settle(ctx); err != nil {
		return err
	}
	o, err := deploymentAction(ctx, e, r, "inventory", func() (*Outcome, error) {
		return e.mcp(ctx, "agent scale_workload inventory without audit", "scale_workload", args("namespace", "shop", "deployment", "inventory", "replicas", 3), false)
	})
	if err != nil {
		return err
	}
	c := coverage(o.Graph, "kubernetes-audit")
	r.check("audit coverage reported as missing", !c.Available, "%s", c.Detail)
	viaResponse := false
	for _, ed := range o.Graph.Edges {
		if ed.Rule == correlate.RuleResponseUID {
			viaResponse = true
		}
	}
	r.check("DIRECT evidence from the client response", viaResponse, "rule %s", correlate.RuleResponseUID)
	return nil
}

func missingMetrics(ctx context.Context, e *Env, r *Result) error {
	scale := func(n string) error {
		_, err := e.Lab.KubectlQuiet(ctx, "-n", "observability", "scale", "deployment/prometheus", "--replicas="+n)
		return err
	}
	if err := scale("0"); err != nil {
		return err
	}
	defer func() {
		_ = scale("1")
		_, _ = e.Lab.KubectlQuiet(context.Background(), "-n", "observability", "rollout", "status", "deployment/prometheus", "--timeout=120s")
	}()
	if err := settle(ctx); err != nil {
		return err
	}
	o, err := deploymentAction(ctx, e, r, "payments", func() (*Outcome, error) {
		return e.mcp(ctx, "agent restart_workload payments without metrics", "restart_workload", args("namespace", "shop", "deployment", "payments"), false)
	})
	if err != nil {
		return err
	}
	c := coverage(o.Graph, "prometheus")
	r.check("Prometheus coverage reported as unavailable", !c.Available, "%s", c.Detail)
	r.check("no temporal edges without metrics", o.Eval.EdgeCounts["TEMPORAL_CORRELATION"] == 0, "%d", o.Eval.EdgeCounts["TEMPORAL_CORRELATION"])
	return nil
}

// ClockSkew measures the difference between the host clock and the kind
// node's clock. Experiments compare harness timestamps with API server
// timestamps, so a skewed lab (for example after the host slept) would
// produce misleading results.
func ClockSkew(ctx context.Context) (time.Duration, error) {
	before := time.Now()
	out, err := nodeExec(ctx, "date", "+%s.%N")
	after := time.Now()
	if err != nil {
		return 0, fmt.Errorf("read node clock: %w", err)
	}
	var sec, nsec int64
	if _, err := fmt.Sscanf(out, "%d.%d", &sec, &nsec); err != nil {
		return 0, fmt.Errorf("parse node clock %q: %w", out, err)
	}
	mid := before.Add(after.Sub(before) / 2)
	return time.Unix(sec, nsec).Sub(mid), nil
}

func nodeExec(ctx context.Context, args ...string) (string, error) {
	eng := "docker"
	if _, err := exec.LookPath("docker"); err != nil {
		eng = "podman"
	}
	full := append([]string{"exec", Cluster + "-control-plane"}, args...)
	out, err := exec.CommandContext(ctx, eng, full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func controllerRestart(ctx context.Context, e *Env, r *Result) error {
	_, err := deploymentAction(ctx, e, r, "frontend", func() (*Outcome, error) {
		o, err := e.mcp(ctx, "agent restart_workload frontend (controller restarts)", "restart_workload", args("namespace", "shop", "deployment", "frontend"), false)
		if err != nil {
			return o, err
		}
		id, err := nodeExec(ctx, "crictl", "ps", "--name", "kube-controller-manager", "-q")
		if err != nil || id == "" {
			return o, fmt.Errorf("find kube-controller-manager (%s): %w", id, err)
		}
		if out, err := nodeExec(ctx, "crictl", "stop", strings.Fields(id)[0]); err != nil {
			return o, fmt.Errorf("stop kube-controller-manager (%s): %w", out, err)
		}
		r.note("kube-controller-manager container %s stopped 0 s after the request; kubelet restarted it.", strings.Fields(id)[0][:12])
		return o, nil
	})
	return err
}

func burstPairs(ctx context.Context, e *Env, r *Result) error {
	type pair struct {
		deploy string
		a, b   act
	}
	pairs := []pair{
		{"frontend", act{"agent 1 restart frontend", "frontend", "mcp", "restart_workload", args("namespace", "shop", "deployment", "frontend"), nil},
			act{"agent 2 scale frontend", "frontend", "mcp", "scale_workload", args("namespace", "shop", "deployment", "frontend", "replicas", 3), nil}},
		{"checkout", act{"agent 3 restart checkout", "checkout", "mcp", "restart_workload", args("namespace", "shop", "deployment", "checkout"), nil},
			act{"agent 4 scale checkout", "checkout", "mcp", "scale_workload", args("namespace", "shop", "deployment", "checkout", "replicas", 4), nil}},
		{"payments", act{"agent 5 restart payments", "payments", "mcp", "restart_workload", args("namespace", "shop", "deployment", "payments"), nil},
			act{"agent 6 scale payments", "payments", "mcp", "scale_workload", args("namespace", "shop", "deployment", "payments", "replicas", 3), nil}},
		{"inventory", act{"agent 7 restart inventory", "inventory", "mcp", "restart_workload", args("namespace", "shop", "deployment", "inventory"), nil},
			act{"agent 8 scale inventory", "inventory", "mcp", "scale_workload", args("namespace", "shop", "deployment", "inventory", "replicas", 3), nil}},
	}
	uids := map[string]string{}
	var all []act
	for _, p := range pairs {
		uids[p.deploy] = e.Rec.UIDOf("Deployment", p.deploy)
		all = append(all, p.a, p.b)
	}
	outs := make([]*Outcome, len(all))
	errs := make([]error, len(all))
	var wg sync.WaitGroup
	for i, a := range all {
		wg.Add(1)
		go func() { defer wg.Done(); outs[i], errs[i] = e.run(ctx, a) }()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := e.waitStable(ctx, 4*time.Minute, "frontend", "checkout", "payments", "inventory"); err != nil {
		return err
	}
	if err := settle(ctx); err != nil {
		return err
	}
	end := time.Now()
	for _, o := range outs {
		o.End = end
	}
	err := e.finish(ctx, r, graphTimeout, func(objs map[string]Life) error {
		for i, a := range all {
			o := outs[i]
			// Both actions of a pair act within milliseconds: every
			// descendant change is ambiguous between them.
			o.Truth = Truth{Direct: []string{uids[a.deploy]}, Ambiguous: DescendantTruth(objs, uids[a.deploy], o.Started, end), Workloads: []string{a.deploy}}
		}
		return nil
	}, outs...)
	if err != nil {
		return err
	}
	cross := 0
	for i, o := range outs {
		r.check(o.Label+": no false attachments", len(o.Eval.FalseAttachments) == 0, "%v", o.Eval.FalseAttachments)
		r.check(o.Label+": direct precision and recall", o.Eval.Direct.FP == 0 && o.Eval.Direct.FN == 0, "%+v", o.Eval.Direct)
		for j, p := range outs {
			if i == j || all[i].deploy == all[j].deploy {
				continue
			}
			for _, u := range o.Eval.Claims {
				if slices.Contains(p.Truth.Ambiguous, u) || slices.Contains(p.Truth.Direct, u) {
					cross++
				}
			}
		}
	}
	r.check("no cross-workload attribution", cross == 0, "%d cross-workload claims", cross)
	ok := cross == 0
	r.Concurrent = true
	r.Separated = &ok
	claimedAmbiguous := 0
	for _, o := range outs {
		claimedAmbiguous += len(o.Eval.AmbiguousClaimed)
	}
	r.note("Same-workload pairs: %d attributed claim(s) on changes that are ambiguous between the two actions of a pair.", claimedAmbiguous)
	r.check("ambiguous same-workload changes claimed by no action", claimedAmbiguous == 0, "%d", claimedAmbiguous)
	return nil
}

// hostileTelemetry sends crafted OTLP/JSON through the lab OTel Collector.
func hostileTelemetry(ctx context.Context, e *Env, r *Result) error {
	// A fresh action whose graph must survive the hostile input unchanged.
	// (Earlier graphs may be gone if an earlier experiment restarted the
	// collector.)
	target, err := deploymentAction(ctx, e, r, "checkout", func() (*Outcome, error) {
		return e.mcp(ctx, "agent restart_workload checkout (before hostile input)", "restart_workload", args("namespace", "shop", "deployment", "checkout"), false)
	})
	if err != nil {
		return err
	}
	before, _, err := e.Lab.Get(ctx, "/api/v1/effects/"+target.ActionID)
	if err != nil {
		return err
	}
	rejectedBefore := metricValue(ctx, e, `effecttrace_ingest_records_total{kind="span",result="rejected"}`) + metricValue(ctx, e, `effecttrace_otlp_spans_total{result="rejected"}`)
	auditID := ""
	for _, n := range target.Graph.Nodes {
		if n.Request != nil && n.Request.AuditID != "" {
			auditID = n.Request.AuditID
		}
	}
	now := time.Now().UnixNano()
	ts := func(off time.Duration) string { return fmt.Sprint(now + int64(off)) }
	trace := "7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a"
	attr := func(k, v string) map[string]any {
		return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
	}
	spans := []map[string]any{
		{"traceId": trace, "spanId": "1111111111111111", "name": "tools/call \u001b[2Jwipe", "kind": 2, "startTimeUnixNano": ts(0), "endTimeUnixNano": ts(time.Millisecond),
			"attributes": []any{attr("mcp.method.name", "tools/call")}},
		{"traceId": "00000000000000000000000000000000", "spanId": "2222222222222222", "name": "tools/call zero", "kind": 2, "startTimeUnixNano": ts(0), "endTimeUnixNano": ts(time.Millisecond),
			"attributes": []any{attr("mcp.method.name", "tools/call")}},
		{"traceId": trace, "spanId": "3333333333333333", "name": "tools/call big", "kind": 2, "startTimeUnixNano": ts(0), "endTimeUnixNano": ts(time.Millisecond),
			"attributes": []any{attr("mcp.method.name", "tools/call"), attr("gen_ai.tool.name", strings.Repeat("x", 4000))}},
		{"traceId": trace, "spanId": "4444444444444444", "name": "tools/call <script>alert(1)</script>", "kind": 2, "startTimeUnixNano": ts(0), "endTimeUnixNano": ts(100 * time.Millisecond),
			"attributes": []any{attr("mcp.method.name", "tools/call"), attr("gen_ai.tool.name", "<script>alert(1)</script>")}},
		// Forged client span naming a real audit ID with the wrong verb.
		{"traceId": trace, "spanId": "5555555555555555", "parentSpanId": "4444444444444444", "name": "delete pods", "kind": 3, "startTimeUnixNano": ts(time.Millisecond), "endTimeUnixNano": ts(2 * time.Millisecond),
			"attributes": []any{attr("effecttrace.k8s.verb", "delete"), attr("effecttrace.k8s.resource", "pods"), attr("effecttrace.k8s.object.namespace", "shop"),
				attr("effecttrace.k8s.object.name", "checkout"), attr("effecttrace.k8s.audit_id", auditID)}},
	}
	body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []any{attr("service.name", "forged-producer")}},
		"scopeSpans": []any{map[string]any{"spans": spans}},
	}}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, OTLPURL+"/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.Lab.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if err := pause(ctx, 8*time.Second); err != nil {
		return err
	}
	if _, _, err := e.Lab.Get(ctx, "/healthz"); err != nil {
		r.check("collector healthy after hostile input", false, "%v", err)
		return nil
	}
	r.check("collector healthy after hostile input", true, "")
	rejectedAfter := metricValue(ctx, e, `effecttrace_ingest_records_total{kind="span",result="rejected"}`) + metricValue(ctx, e, `effecttrace_otlp_spans_total{result="rejected"}`)
	r.check("invalid spans rejected", rejectedAfter-rejectedBefore >= 3, "rejected counter +%.0f", rejectedAfter-rejectedBefore)
	after, _, err := e.Lab.Get(ctx, "/api/v1/effects/"+target.ActionID)
	if err != nil {
		return err
	}
	r.check("existing graph unchanged", bytes.Equal(before, after), "graph of %s compared byte for byte", target.ActionID)
	id, err := e.Lab.ActionForTrace(ctx, trace, 20*time.Second)
	if err != nil {
		return err
	}
	raw, _, err := e.Lab.Get(ctx, "/api/v1/effects/"+id)
	if err != nil {
		return err
	}
	r.check("HTML escaped in API output", !bytes.Contains(raw, []byte("<script>")) && bytes.Contains(raw, []byte(`\u003cscript\u003e`)), "JSON encoder escapes < and >")
	g, err := model.UnmarshalGraph(raw)
	if err != nil {
		return err
	}
	confirmed := false
	for _, n := range g.Nodes {
		if n.Request != nil && n.Request.AuditID == auditID {
			confirmed = true
		}
	}
	direct := 0
	for _, ed := range g.Edges {
		if ed.Evidence == model.EvidenceDirect {
			direct++
		}
	}
	r.check("forged span not confirmed by the real audit event", !confirmed, "audit ID %s", auditID)
	r.check("forged action has no DIRECT claim", direct == 0, "%d DIRECT edges", direct)
	r.note("A producer that forges a span with a correct audit ID AND matching verb and object would be accepted: EffectTrace trusts its telemetry producers (see docs/threat-model.md).")
	return nil
}

func metricValue(ctx context.Context, e *Env, series string) float64 {
	b, _, err := e.Lab.Get(ctx, "/metrics")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, series+" ") {
			var v float64
			fmt.Sscanf(strings.TrimPrefix(line, series+" "), "%g", &v)
			return v
		}
	}
	return 0
}
