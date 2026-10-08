// Package kube watches workload objects and Events with client-go informers
// and converts notifications into observations.
//
// Objects are stripped to identity, ownership and rollout status by an
// informer transform BEFORE they enter the informer cache, so Pod specs,
// environment variables and container commands are never retained.
// ConfigMaps and Secrets are never watched.
package kube

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/privacy"
	"github.com/effecttrace/effecttrace/pkg/model"
)

// Sink receives records and blocks while the pipeline is full.
type Sink func(ctx context.Context, r obs.Record) error

// Watcher runs the informers.
type Watcher struct {
	Client     kubernetes.Interface
	Namespaces []string
	Logger     *slog.Logger
	Sink       Sink
	// Now returns the observation time; tests may override it.
	Now func() time.Time
}

// Run starts informers and blocks until ctx is cancelled. synced is closed
// once all caches have synced.
func (w *Watcher) Run(ctx context.Context, synced chan<- struct{}) error {
	if w.Now == nil {
		w.Now = func() time.Time { return time.Now().UTC() }
	}
	started := w.Now()
	var factories []informers.SharedInformerFactory
	opts := []informers.SharedInformerOption{informers.WithTransform(Strip)}
	if len(w.Namespaces) == 0 {
		factories = append(factories, informers.NewSharedInformerFactoryWithOptions(w.Client, 0, opts...))
	} else {
		for _, ns := range w.Namespaces {
			factories = append(factories, informers.NewSharedInformerFactoryWithOptions(w.Client, 0, append(opts, informers.WithNamespace(ns))...))
		}
	}
	var syncs []cache.InformerSynced
	for _, f := range factories {
		for _, inf := range []cache.SharedIndexInformer{
			f.Apps().V1().Deployments().Informer(),
			f.Apps().V1().ReplicaSets().Informer(),
			f.Apps().V1().StatefulSets().Informer(),
			f.Apps().V1().DaemonSets().Informer(),
			f.Batch().V1().Jobs().Informer(),
			f.Core().V1().Pods().Informer(),
			f.Core().V1().Services().Informer(),
		} {
			reg, err := inf.AddEventHandler(w.objectHandler(ctx))
			if err != nil {
				return err
			}
			syncs = append(syncs, reg.HasSynced)
		}
		reg, err := f.Core().V1().Events().Informer().AddEventHandler(w.eventHandler(ctx))
		if err != nil {
			return err
		}
		syncs = append(syncs, reg.HasSynced)
		f.Start(ctx.Done())
	}
	if !cache.WaitForCacheSync(ctx.Done(), syncs...) {
		return ctx.Err()
	}
	for _, s := range []string{"kubernetes-watch", "kubernetes-events"} {
		_ = w.Sink(ctx, obs.Record{Kind: obs.KindSource, Source: &obs.SourceStatus{
			Source: s, At: w.Now(), Healthy: true, StartedAt: started, Detail: "informers synced",
		}})
	}
	if synced != nil {
		close(synced)
	}
	<-ctx.Done()
	for _, f := range factories {
		f.Shutdown()
	}
	return nil
}

func (w *Watcher) emit(ctx context.Context, obj any, typ obs.WatchType, initial bool) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	o, ok := Observe(obj, typ, initial, w.Now())
	if !ok {
		return
	}
	if err := w.Sink(ctx, obs.Record{Kind: obs.KindObject, Object: o}); err != nil && ctx.Err() == nil {
		w.Logger.Warn("dropping object observation", "err", err)
	}
}

func (w *Watcher) objectHandler(ctx context.Context) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc:    func(obj any, initial bool) { w.emit(ctx, obj, obs.WatchAdded, initial) },
		UpdateFunc: func(_, obj any) { w.emit(ctx, obj, obs.WatchModified, false) },
		DeleteFunc: func(obj any) { w.emit(ctx, obj, obs.WatchDeleted, false) },
	}
}

func (w *Watcher) eventHandler(ctx context.Context) cache.ResourceEventHandler {
	send := func(obj any, initial bool) {
		ev, ok := obj.(*corev1.Event)
		if !ok {
			return
		}
		rec := EventObservation(ev, initial, w.Now())
		if rec == nil {
			return
		}
		if err := w.Sink(ctx, obs.Record{Kind: obs.KindEvent, Event: rec}); err != nil && ctx.Err() == nil {
			w.Logger.Warn("dropping event observation", "err", err)
		}
	}
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc:    send,
		UpdateFunc: func(_, obj any) { send(obj, false) },
	}
}

// Annotation and label keys read from workload objects.
const (
	annRevision       = "deployment.kubernetes.io/revision"
	labelTemplateHash = "pod-template-hash"
)

func stripMeta(m metav1.ObjectMeta, keepAnnotation, keepLabel string) metav1.ObjectMeta {
	out := metav1.ObjectMeta{
		Name: m.Name, Namespace: m.Namespace, UID: m.UID, ResourceVersion: m.ResourceVersion,
		Generation: m.Generation, CreationTimestamp: m.CreationTimestamp, DeletionTimestamp: m.DeletionTimestamp,
		OwnerReferences: m.OwnerReferences,
	}
	if v, ok := m.Annotations[keepAnnotation]; ok && keepAnnotation != "" {
		out.Annotations = map[string]string{keepAnnotation: v}
	}
	if v, ok := m.Labels[keepLabel]; ok && keepLabel != "" {
		out.Labels = map[string]string{keepLabel: v}
	}
	return out
}

// Strip is an informer transform that removes everything EffectTrace does
// not use before the object is cached.
func Strip(obj any) (any, error) {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return &appsv1.Deployment{TypeMeta: o.TypeMeta, ObjectMeta: stripMeta(o.ObjectMeta, annRevision, ""),
			Spec: appsv1.DeploymentSpec{Replicas: o.Spec.Replicas}, Status: o.Status}, nil
	case *appsv1.ReplicaSet:
		return &appsv1.ReplicaSet{TypeMeta: o.TypeMeta, ObjectMeta: stripMeta(o.ObjectMeta, annRevision, labelTemplateHash),
			Spec: appsv1.ReplicaSetSpec{Replicas: o.Spec.Replicas}, Status: o.Status}, nil
	case *appsv1.StatefulSet:
		return &appsv1.StatefulSet{TypeMeta: o.TypeMeta, ObjectMeta: stripMeta(o.ObjectMeta, "", ""),
			Spec: appsv1.StatefulSetSpec{Replicas: o.Spec.Replicas}, Status: o.Status}, nil
	case *appsv1.DaemonSet:
		return &appsv1.DaemonSet{TypeMeta: o.TypeMeta, ObjectMeta: stripMeta(o.ObjectMeta, "", ""), Status: o.Status}, nil
	case *batchv1.Job:
		return &batchv1.Job{TypeMeta: o.TypeMeta, ObjectMeta: stripMeta(o.ObjectMeta, "", ""), Status: batchv1.JobStatus{
			Active: o.Status.Active, Succeeded: o.Status.Succeeded, Failed: o.Status.Failed}}, nil
	case *corev1.Pod:
		var conds []corev1.PodCondition
		for _, c := range o.Status.Conditions {
			if c.Type == corev1.PodReady {
				conds = append(conds, corev1.PodCondition{Type: c.Type, Status: c.Status, LastTransitionTime: c.LastTransitionTime})
			}
		}
		return &corev1.Pod{TypeMeta: o.TypeMeta, ObjectMeta: stripMeta(o.ObjectMeta, "", labelTemplateHash),
			Status: corev1.PodStatus{Phase: o.Status.Phase, Conditions: conds}}, nil
	case *corev1.Service:
		return &corev1.Service{TypeMeta: o.TypeMeta, ObjectMeta: stripMeta(o.ObjectMeta, "", "")}, nil
	case *corev1.Event:
		return &corev1.Event{TypeMeta: o.TypeMeta, ObjectMeta: metav1.ObjectMeta{Name: o.Name, Namespace: o.Namespace, UID: o.UID, ResourceVersion: o.ResourceVersion},
			InvolvedObject: corev1.ObjectReference{Kind: o.InvolvedObject.Kind, Namespace: o.InvolvedObject.Namespace, Name: o.InvolvedObject.Name, UID: o.InvolvedObject.UID, APIVersion: o.InvolvedObject.APIVersion},
			Reason:         o.Reason, Message: o.Message, Type: o.Type, Count: o.Count, Source: corev1.EventSource{Component: o.Source.Component},
			ReportingController: o.ReportingController, FirstTimestamp: o.FirstTimestamp, LastTimestamp: o.LastTimestamp, EventTime: o.EventTime,
			Series: o.Series}, nil
	}
	return obj, nil
}

func owners(refs []metav1.OwnerReference) []obs.OwnerRef {
	var out []obs.OwnerRef
	for _, r := range refs {
		out = append(out, obs.OwnerRef{Kind: r.Kind, Name: r.Name, UID: string(r.UID), Controller: r.Controller != nil && *r.Controller})
	}
	return out
}

func base(m metav1.ObjectMeta, apiVersion, kind string, typ obs.WatchType, initial bool, at time.Time) *obs.ObjectObservation {
	o := &obs.ObjectObservation{
		At: at, Type: typ, Initial: initial, ResourceVersion: m.ResourceVersion, Generation: m.Generation,
		Ref:       model.ObjectRef{APIVersion: apiVersion, Kind: kind, Namespace: m.Namespace, Name: m.Name, UID: string(m.UID)},
		CreatedAt: m.CreationTimestamp.UTC(), Owners: owners(m.OwnerReferences),
	}
	if m.DeletionTimestamp != nil {
		o.DeletingAt = m.DeletionTimestamp.UTC()
	}
	return o
}

func i32(v int32) *int32 { return &v }

// Observe converts a (stripped) object into an observation.
func Observe(obj any, typ obs.WatchType, initial bool, at time.Time) (*obs.ObjectObservation, bool) {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		r := base(o.ObjectMeta, "apps/v1", "Deployment", typ, initial, at)
		r.Replicas = o.Spec.Replicas
		r.ObservedGeneration = o.Status.ObservedGeneration
		r.StatusReplicas, r.ReadyReplicas = i32(o.Status.Replicas), i32(o.Status.ReadyReplicas)
		r.UpdatedReplicas, r.AvailableReplicas = i32(o.Status.UpdatedReplicas), i32(o.Status.AvailableReplicas)
		r.Revision = o.Annotations[annRevision]
		return r, true
	case *appsv1.ReplicaSet:
		r := base(o.ObjectMeta, "apps/v1", "ReplicaSet", typ, initial, at)
		r.Replicas = o.Spec.Replicas
		r.ObservedGeneration = o.Status.ObservedGeneration
		r.StatusReplicas, r.ReadyReplicas = i32(o.Status.Replicas), i32(o.Status.ReadyReplicas)
		r.AvailableReplicas = i32(o.Status.AvailableReplicas)
		r.Revision = o.Annotations[annRevision]
		r.TemplateHash = o.Labels[labelTemplateHash]
		return r, true
	case *appsv1.StatefulSet:
		r := base(o.ObjectMeta, "apps/v1", "StatefulSet", typ, initial, at)
		r.Replicas = o.Spec.Replicas
		r.ObservedGeneration = o.Status.ObservedGeneration
		r.StatusReplicas, r.ReadyReplicas = i32(o.Status.Replicas), i32(o.Status.ReadyReplicas)
		r.UpdatedReplicas, r.AvailableReplicas = i32(o.Status.UpdatedReplicas), i32(o.Status.AvailableReplicas)
		return r, true
	case *appsv1.DaemonSet:
		r := base(o.ObjectMeta, "apps/v1", "DaemonSet", typ, initial, at)
		r.ObservedGeneration = o.Status.ObservedGeneration
		r.Replicas = i32(o.Status.DesiredNumberScheduled)
		r.ReadyReplicas, r.UpdatedReplicas = i32(o.Status.NumberReady), i32(o.Status.UpdatedNumberScheduled)
		r.AvailableReplicas = i32(o.Status.NumberAvailable)
		r.StatusReplicas = i32(o.Status.CurrentNumberScheduled)
		return r, true
	case *batchv1.Job:
		return base(o.ObjectMeta, "batch/v1", "Job", typ, initial, at), true
	case *corev1.Pod:
		r := base(o.ObjectMeta, "v1", "Pod", typ, initial, at)
		ready := false
		for _, c := range o.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		r.Ready = &ready
		r.TemplateHash = o.Labels[labelTemplateHash]
		return r, true
	case *corev1.Service:
		return base(o.ObjectMeta, "v1", "Service", typ, initial, at), true
	}
	return nil, false
}

// MaxEventNote bounds the retained Event message.
const MaxEventNote = 256

// EventObservation converts a core/v1 Event.
func EventObservation(ev *corev1.Event, initial bool, at time.Time) *obs.EventObservation {
	if ev.InvolvedObject.UID == "" || ev.Reason == "" {
		return nil
	}
	when := at
	if initial {
		switch {
		case !ev.EventTime.IsZero():
			when = ev.EventTime.UTC()
		case !ev.LastTimestamp.IsZero():
			when = ev.LastTimestamp.UTC()
		default:
			when = ev.CreationTimestamp.UTC()
		}
	}
	controller := ev.ReportingController
	if controller == "" {
		controller = ev.Source.Component
	}
	count := ev.Count
	if ev.Series != nil && ev.Series.Count > count {
		count = ev.Series.Count
	}
	return &obs.EventObservation{
		UID: string(ev.UID), At: when,
		Regarding: model.ObjectRef{APIVersion: ev.InvolvedObject.APIVersion, Kind: ev.InvolvedObject.Kind,
			Namespace: ev.InvolvedObject.Namespace, Name: ev.InvolvedObject.Name, UID: string(ev.InvolvedObject.UID)},
		Reason: privacy.Text(ev.Reason, 128), Type: privacy.Text(ev.Type, 32),
		Note: privacy.Text(ev.Message, MaxEventNote), Controller: privacy.Text(controller, 128), Count: count,
		Initial: initial,
	}
}

// String describes the watcher configuration.
func (w *Watcher) String() string {
	if len(w.Namespaces) == 0 {
		return "all namespaces"
	}
	return fmt.Sprintf("namespaces %v", w.Namespaces)
}
