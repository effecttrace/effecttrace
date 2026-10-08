package kube

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/effecttrace/effecttrace/internal/obs"
)

func secretPod() *corev1.Pod {
	ctrl := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-abc-1", Namespace: "shop", UID: "pod-uid", ResourceVersion: "10",
			Labels:          map[string]string{"pod-template-hash": "abc", "team": "payments-secret-team"},
			Annotations:     map[string]string{"note": "internal"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "checkout-abc", UID: "rs-uid", Controller: &ctrl}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.example/app:1",
			Env: []corev1.EnvVar{{Name: "DB_PASSWORD", Value: "hunter2"}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue}, {Type: corev1.PodScheduled, Status: corev1.ConditionTrue, Message: "x"}}},
	}
}

func TestStripRemovesSpecAndEnv(t *testing.T) {
	out, err := Strip(secretPod())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	for _, leak := range []string{"hunter2", "DB_PASSWORD", "registry.example", "payments-secret-team", "internal"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("stripped pod still contains %q: %s", leak, b)
		}
	}
	o, ok := Observe(out, obs.WatchAdded, false, time.Now())
	if !ok || o.Ready == nil || !*o.Ready || o.TemplateHash != "abc" || len(o.Owners) != 1 || !o.Owners[0].Controller {
		t.Fatalf("unexpected observation: %+v", o)
	}
}

func TestEventObservationSanitizes(t *testing.T) {
	ev := &corev1.Event{ObjectMeta: metav1.ObjectMeta{UID: "ev"}, InvolvedObject: corev1.ObjectReference{Kind: "Deployment", UID: "d"},
		Reason: "ScalingReplicaSet", Message: "Scaled up\x1b[2J replica set " + strings.Repeat("x", 600), Source: corev1.EventSource{Component: "deployment-controller"}, Count: 2}
	r := EventObservation(ev, false, time.Now())
	if strings.ContainsRune(r.Note, 0x1b) || len(r.Note) > MaxEventNote || r.Controller != "deployment-controller" {
		t.Fatalf("event not sanitized: %+v", r)
	}
	if EventObservation(&corev1.Event{Reason: "x"}, false, time.Now()) != nil {
		t.Fatal("event without involved UID accepted")
	}
}

func TestWatcherEmitsObservations(t *testing.T) {
	replicas := int32(2)
	client := fake.NewClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", UID: "dep-uid", ResourceVersion: "1", Generation: 3},
			Spec: appsv1.DeploymentSpec{Replicas: &replicas}},
		secretPod(),
	)
	var mu sync.Mutex
	var recs []obs.Record
	synced := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := &Watcher{Client: client, Logger: slog.New(slog.DiscardHandler), Sink: func(_ context.Context, r obs.Record) error {
		mu.Lock()
		recs = append(recs, r)
		mu.Unlock()
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, synced) }()
	select {
	case <-synced:
	case <-ctx.Done():
		t.Fatal("informers did not sync")
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	var dep, pod, src bool
	for _, r := range recs {
		switch {
		case r.Kind == obs.KindObject && r.Object.Ref.Kind == "Deployment":
			dep = r.Object.Initial && r.Object.Generation == 3 && *r.Object.Replicas == 2
		case r.Kind == obs.KindObject && r.Object.Ref.Kind == "Pod":
			pod = r.Object.Initial
		case r.Kind == obs.KindSource && r.Source.Source == "kubernetes-watch":
			src = r.Source.Healthy
		}
	}
	if !dep || !pod || !src {
		t.Fatalf("deployment=%v pod=%v source=%v in %d records", dep, pod, src, len(recs))
	}
}
