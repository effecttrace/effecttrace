// Package integration wires EffectTrace's real components together in one
// process: the audit tailer on a temporary file, the OTLP/HTTP receiver, the
// Kubernetes watcher on a fake clientset, the ingest pipeline, the
// correlation engine and the HTTP API. No cluster is required.
package integration

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/effecttrace/effecttrace/internal/api"
	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/pipeline"
	"github.com/effecttrace/effecttrace/internal/privacy"
	"github.com/effecttrace/effecttrace/internal/source/audit"
	"github.com/effecttrace/effecttrace/internal/source/kube"
	"github.com/effecttrace/effecttrace/internal/source/otlp"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/pkg/model"
	"github.com/effecttrace/effecttrace/pkg/semconv"
)

func i32(v int32) *int32 { return &v }

var ctrl = true

func owner(kind, name string, uid types.UID) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: uid, Controller: &ctrl}}
}

func deployment(gen, obsGen int64, rv string, stable bool) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", UID: "dep-uid", ResourceVersion: rv, Generation: gen},
		Spec:       appsv1.DeploymentSpec{Replicas: i32(2)},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: obsGen},
	}
	if stable {
		d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas, d.Status.ReadyReplicas = 2, 2, 2, 2
	}
	return d
}

func rs(name string, uid types.UID, replicas int32, rv string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: uid, ResourceVersion: rv, Generation: 1,
		OwnerReferences: owner("Deployment", "checkout", "dep-uid")}, Spec: appsv1.ReplicaSetSpec{Replicas: i32(replicas)},
		Status: appsv1.ReplicaSetStatus{Replicas: replicas, ReadyReplicas: replicas}}
}

func pod(name string, uid types.UID, rsName string, rsUID types.UID) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: uid, ResourceVersion: "1",
		OwnerReferences: owner("ReplicaSet", rsName, rsUID)},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
}

func kv(k, v string) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: v}}}
}

func TestCollectorEndToEndInProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	logger := slog.New(slog.DiscardHandler)

	st := store.New(store.DefaultConfig())
	dir := t.TempDir()
	rec, err := pipeline.NewRecorder(filepath.Join(dir, "recording.jsonl"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	pl := pipeline.New(st, 1000, rec, nil, logger)
	go pl.Run(ctx)

	cfg := correlate.DefaultConfig()
	cfg.Settle = 200 * time.Millisecond
	engine := correlate.NewEngine(st, cfg, correlate.Options{})

	// Kubernetes: a stable Deployment with one ReplicaSet and two Pods.
	client := fake.NewClientset(deployment(1, 1, "10", true), rs("checkout-old", "rs-old", 2, "10"),
		pod("checkout-old-a", "pod-a", "checkout-old", "rs-old"), pod("checkout-old-b", "pod-b", "checkout-old", "rs-old"))
	synced := make(chan struct{})
	w := &kube.Watcher{Client: client, Logger: logger, Sink: pl.Submit}
	go func() { _ = w.Run(ctx, synced) }()
	select {
	case <-synced:
	case <-ctx.Done():
		t.Fatal("informers did not sync")
	}

	// Audit log tailer on a temporary file.
	auditPath := filepath.Join(dir, "audit.log")
	if err := os.WriteFile(auditPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tl := &audit.Tailer{Path: auditPath, Poll: 20 * time.Millisecond, Policy: privacy.Policy{Mode: privacy.IdentityPseudonymize}, Logger: logger, Sink: pl.Submit}
	go func() { _ = tl.Run(ctx) }()

	otlpSrv := httptest.NewServer(&otlp.Receiver{Offer: pl.Offer, Logger: logger})
	defer otlpSrv.Close()
	apiSrv := httptest.NewServer((&api.Server{Engine: engine, Store: st, Version: "test"}).Handler(nil))
	defer apiSrv.Close()

	// The tool's PATCH: audit event, watch notification, spans.
	t0 := time.Now().UTC()
	auditLine := fmt.Sprintf(`{"kind":"Event","apiVersion":"audit.k8s.io/v1","level":"Metadata","auditID":"aud-1","stage":"ResponseComplete","requestURI":"/apis/apps/v1/namespaces/shop/deployments/checkout","verb":"patch","user":{"username":"system:serviceaccount:effecttrace-demo:demo-actor"},"userAgent":"demo","objectRef":{"resource":"deployments","namespace":"shop","name":"checkout","apiGroup":"apps","apiVersion":"v1"},"responseStatus":{"code":200},"requestReceivedTimestamp":%q,"stageTimestamp":%q}`+"\n",
		t0.Format(time.RFC3339Nano), t0.Add(5*time.Millisecond).Format(time.RFC3339Nano))
	f, _ := os.OpenFile(auditPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(auditLine)
	f.Close()
	if _, err := client.AppsV1().Deployments("shop").Update(ctx, deployment(2, 1, "11", true), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	traceID, _ := hex.DecodeString("0af7651916cd43dd8448eb211c80319c")
	toolSpan, _ := hex.DecodeString("b7ad6b7169203331")
	clientSpan, _ := hex.DecodeString("00f067aa0ba902b7")
	ns := func(t time.Time) uint64 { return uint64(t.UnixNano()) }
	req := &coltrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resource.Resource{Attributes: []*common.KeyValue{kv("service.name", "demo-tools")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
			{TraceId: traceID, SpanId: toolSpan, Name: "tools/call restart_workload", Kind: tracepb.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano: ns(t0.Add(-5 * time.Millisecond)), EndTimeUnixNano: ns(t0.Add(20 * time.Millisecond)),
				Attributes: []*common.KeyValue{kv(semconv.MCPMethodName, "tools/call"), kv(semconv.GenAIToolName, "restart_workload")}},
			{TraceId: traceID, SpanId: clientSpan, ParentSpanId: toolSpan, Name: "patch deployments", Kind: tracepb.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: ns(t0.Add(-time.Millisecond)), EndTimeUnixNano: ns(t0.Add(8 * time.Millisecond)),
				Attributes: []*common.KeyValue{kv(semconv.K8sVerb, "patch"), kv(semconv.K8sResource, "deployments"), kv(semconv.K8sAPIGroup, "apps"),
					kv(semconv.K8sObjectNamespace, "shop"), kv(semconv.K8sObjectName, "checkout"), kv(semconv.K8sAuditID, "aud-1"),
					kv(semconv.K8sObjectUID, "dep-uid"), kv(semconv.K8sObjectResourceVersion, "11"), kv(semconv.K8sObjectGeneration, "2"),
					kv(semconv.HTTPResponseStatusCode, "200")}},
		}}},
	}}}
	body, _ := proto.Marshal(req)
	resp, err := http.Post(otlpSrv.URL+"/v1/traces", "application/x-protobuf", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("OTLP export: %v %v", err, resp)
	}
	resp.Body.Close()

	// The controllers react: new ReplicaSet and Pods, old Pods deleted.
	time.Sleep(50 * time.Millisecond) // order the simulated controller after the request
	client.AppsV1().ReplicaSets("shop").Create(ctx, rs("checkout-new", "rs-new", 2, "12"), metav1.CreateOptions{})
	client.CoreV1().Pods("shop").Create(ctx, pod("checkout-new-a", "pod-c", "checkout-new", "rs-new"), metav1.CreateOptions{})
	client.CoreV1().Pods("shop").Create(ctx, pod("checkout-new-b", "pod-d", "checkout-new", "rs-new"), metav1.CreateOptions{})
	client.CoreV1().Pods("shop").Delete(ctx, "checkout-old-a", metav1.DeleteOptions{})
	client.CoreV1().Pods("shop").Delete(ctx, "checkout-old-b", metav1.DeleteOptions{})
	old := rs("checkout-old", "rs-old", 0, "13")
	client.AppsV1().ReplicaSets("shop").Update(ctx, old, metav1.UpdateOptions{})
	client.AppsV1().Deployments("shop").Update(ctx, deployment(2, 2, "14", true), metav1.UpdateOptions{})

	// Poll the API until the graph is complete.
	var g *model.EffectGraph
	for {
		r, err := http.Get(apiSrv.URL + "/api/v1/traces/0af7651916cd43dd8448eb211c80319c/actions")
		if err == nil {
			var ids struct {
				Actions []string `json:"actions"`
			}
			json.NewDecoder(r.Body).Decode(&ids)
			r.Body.Close()
			if len(ids.Actions) == 1 {
				gr, err := http.Get(apiSrv.URL + "/api/v1/effects/" + ids.Actions[0])
				if err == nil {
					var buf bytes.Buffer
					buf.ReadFrom(gr.Body)
					gr.Body.Close()
					if gg, err := model.UnmarshalGraph(buf.Bytes()); err == nil && gg.Status == model.StatusComplete && len(gg.Nodes) >= 7 {
						g = gg
						break
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("graph not complete: last=%+v", g)
		case <-time.After(100 * time.Millisecond):
		}
	}
	change := map[string]string{}
	for _, n := range g.Nodes {
		if n.Object != nil && n.Change != nil {
			change[n.Object.UID] = n.Change.Type
		}
	}
	want := map[string]string{"dep-uid": "MUTATED", "rs-new": "CREATED", "pod-c": "CREATED", "pod-d": "CREATED", "pod-a": "DELETED", "pod-b": "DELETED", "rs-old": "SCALED"}
	for uid, typ := range want {
		if change[uid] != typ {
			t.Errorf("%s: change %q, want %q (all: %v)", uid, change[uid], typ, change)
		}
	}
	direct := false
	for _, e := range g.Edges {
		if e.Evidence == model.EvidenceDirect && e.Rule == correlate.RuleResponseUID {
			for _, fct := range e.Facts {
				if fct.Key == "audit_id_match" {
					direct = true
				}
			}
		}
	}
	if !direct {
		t.Error("DIRECT edge without response UID and audit confirmation")
	}
	if err := rec.Flush(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "recording.jsonl"))
	if fi.Size() == 0 {
		t.Error("recording is empty")
	}
}
