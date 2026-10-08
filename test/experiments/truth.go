package experiments

import (
	"context"
	"slices"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Recorder is the harness's independent view of the cluster. It is a
// deliberately simple watch, separate from EffectTrace's code, that records
// object lifetimes on the harness clock.
type Recorder struct {
	mu   sync.Mutex
	objs map[string]*Life
}

// Life is the observed lifetime of one object.
type Life struct {
	UID      string
	Kind     string
	Name     string
	Owner    string // controller owner UID
	Seen     time.Time
	Deleted  time.Time
	Replicas []ReplicaChange
	Initial  bool
}

// ReplicaChange is a desired-replica change of a ReplicaSet.
type ReplicaChange struct {
	At       time.Time
	From, To int32
}

// StartRecorder watches Deployments, ReplicaSets, Pods, Services and
// ConfigMaps (metadata only) in the shop namespace.
func StartRecorder(ctx context.Context, client kubernetes.Interface) (*Recorder, error) {
	r := &Recorder{objs: map[string]*Life{}}
	f := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(Namespace))
	handler := func(kind string) cache.ResourceEventHandler {
		return cache.ResourceEventHandlerDetailedFuncs{
			AddFunc:    func(obj any, initial bool) { r.observe(kind, obj, initial, false) },
			UpdateFunc: func(_, obj any) { r.observe(kind, obj, false, false) },
			DeleteFunc: func(obj any) {
				if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					obj = d.Obj
				}
				r.observe(kind, obj, false, true)
			},
		}
	}
	for kind, inf := range map[string]cache.SharedIndexInformer{
		"Deployment": f.Apps().V1().Deployments().Informer(),
		"ReplicaSet": f.Apps().V1().ReplicaSets().Informer(),
		"Pod":        f.Core().V1().Pods().Informer(),
		"Service":    f.Core().V1().Services().Informer(),
		"ConfigMap":  f.Core().V1().ConfigMaps().Informer(),
	} {
		if _, err := inf.AddEventHandler(handler(kind)); err != nil {
			return nil, err
		}
	}
	f.Start(ctx.Done())
	for _, ok := range f.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return nil, ctx.Err()
		}
	}
	return r, nil
}

func (r *Recorder) observe(kind string, obj any, initial, deleted bool) {
	m, ok := obj.(metav1.Object)
	if !ok {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.objs[string(m.GetUID())]
	if !ok {
		l = &Life{UID: string(m.GetUID()), Kind: kind, Name: m.GetName(), Seen: now, Initial: initial}
		if c := metav1.GetControllerOf(m); c != nil {
			l.Owner = string(c.UID)
		}
		r.objs[l.UID] = l
	}
	if (deleted || m.GetDeletionTimestamp() != nil) && l.Deleted.IsZero() {
		l.Deleted = now
	}
	if rs, ok := obj.(*appsv1.ReplicaSet); ok && rs.Spec.Replicas != nil {
		cur := *rs.Spec.Replicas
		if n := len(l.Replicas); n == 0 {
			l.Replicas = append(l.Replicas, ReplicaChange{At: now, From: cur, To: cur})
		} else if last := l.Replicas[n-1].To; last != cur {
			l.Replicas = append(l.Replicas, ReplicaChange{At: now, From: last, To: cur})
		}
	}
}

// Snapshot returns a copy of all lifetimes.
func (r *Recorder) Snapshot() map[string]Life {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Life, len(r.objs))
	for k, v := range r.objs {
		c := *v
		c.Replicas = slices.Clone(v.Replicas)
		out[k] = c
	}
	return out
}

// UIDOf returns the UID of the live object of a kind and name.
func (r *Recorder) UIDOf(kind, name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *Life
	for _, l := range r.objs {
		if l.Kind == kind && l.Name == name && l.Deleted.IsZero() {
			if best == nil || l.Seen.After(best.Seen) {
				best = l
			}
		}
	}
	if best == nil {
		return ""
	}
	return best.UID
}

// descendantOf reports whether uid is (transitively) controlled by root.
func descendantOf(objs map[string]Life, uid, root string) bool {
	cur := uid
	for range 6 {
		l, ok := objs[cur]
		if !ok || l.Owner == "" {
			return false
		}
		if l.Owner == root {
			return true
		}
		cur = l.Owner
	}
	return false
}

// changedIn reports whether an object was created, deleted or (for a
// ReplicaSet) rescaled in [from, to).
func changedIn(l Life, from, to time.Time) bool {
	in := func(t time.Time) bool { return !t.IsZero() && !t.Before(from) && t.Before(to) }
	if (!l.Initial && in(l.Seen)) || in(l.Deleted) {
		return true
	}
	for i, c := range l.Replicas {
		if i > 0 && in(c.At) {
			return true
		}
	}
	return false
}

// Truth is the ground truth of one action.
type Truth struct {
	Direct     []string `json:"direct"`     // UIDs the action mutated directly
	Structural []string `json:"structural"` // UIDs whose change the action caused through controllers
	// Ambiguous are changes inside the scope of this and another action;
	// attributing them to either action is not counted as correct.
	Ambiguous []string `json:"ambiguous"`
	// Uncertain are objects that changed while a competing request was in
	// flight, when the harness cannot know on which side of the API
	// server's receive time the change fell. They are not scored.
	Uncertain []string `json:"uncertain,omitempty"`
	// Workloads are the workload names whose telemetry is in scope.
	Workloads []string `json:"workloads"`
}

// DescendantTruth returns descendants of root changed in [from, to).
func DescendantTruth(objs map[string]Life, root string, from, to time.Time) []string {
	var out []string
	for uid, l := range objs {
		if l.Kind == "Deployment" || l.Kind == "Service" || l.Kind == "ConfigMap" {
			continue
		}
		if descendantOf(objs, uid, root) && changedIn(l, from, to) {
			out = append(out, uid)
		}
	}
	slices.Sort(out)
	return out
}

// ReplacementTruth returns Pods owned by owner created in [from, to), other
// than the deleted pod.
func ReplacementTruth(objs map[string]Life, owner, deleted string, from, to time.Time) []string {
	var out []string
	for uid, l := range objs {
		if l.Kind == "Pod" && l.Owner == owner && uid != deleted && !l.Initial && !l.Seen.Before(from) && l.Seen.Before(to) {
			out = append(out, uid)
		}
	}
	slices.Sort(out)
	return out
}

// ChangedInNamespace returns every object that changed in [from, to).
func ChangedInNamespace(objs map[string]Life, from, to time.Time) []string {
	var out []string
	for uid, l := range objs {
		if changedIn(l, from, to) {
			out = append(out, uid)
		}
	}
	slices.Sort(out)
	return out
}
