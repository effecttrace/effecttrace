// Package synth generates deterministic synthetic observation streams that
// mimic the Deployment and ReplicaSet controllers. It is used by unit tests,
// fuzz seeds and the synthetic throughput benchmarks. It is NOT evidence of
// real controller behaviour; the kind experiments provide that.
package synth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/pkg/model"
	"github.com/effecttrace/effecttrace/pkg/semconv"
)

// Cluster is a deterministic simulated cluster clock and object factory.
type Cluster struct {
	Now  time.Time
	seq  int
	rv   int
	recs []obs.Record
}

// New returns a cluster whose clock starts at start.
func New(start time.Time) *Cluster { return &Cluster{Now: start.UTC()} }

// Records returns all records emitted so far.
func (c *Cluster) Records() []obs.Record { return c.recs }

// Advance moves the clock forward.
func (c *Cluster) Advance(d time.Duration) { c.Now = c.Now.Add(d) }

func (c *Cluster) id(prefix string) string {
	c.seq++
	h := sha256.Sum256([]byte(prefix + strconv.Itoa(c.seq)))
	return hex.EncodeToString(h[:])
}

// UID returns a deterministic UUID-shaped identifier.
func (c *Cluster) UID() string {
	s := c.id("uid")
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// TraceID returns a deterministic 16-byte hex trace ID.
func (c *Cluster) TraceID() string { return c.id("trace")[:32] }

// SpanID returns a deterministic 8-byte hex span ID.
func (c *Cluster) SpanID() string { return c.id("span")[:16] }

func (c *Cluster) nextRV() string { c.rv++; return strconv.Itoa(1000 + c.rv) }

func i32(v int) *int32 { x := int32(v); return &x } // #nosec G115 -- small synthetic counts

func bptr(b bool) *bool { return &b }

func (c *Cluster) emit(r obs.Record) { c.recs = append(c.recs, r) }

// Object is a simulated object's latest state.
type Object struct {
	Ref        model.ObjectRef
	Owner      *Object
	Generation int64
	ObsGen     int64
	Replicas   int
	Ready      int
	Updated    int
	Revision   string
	Hash       string
	Created    time.Time
	Deleted    bool
	LastRV     string
}

func (c *Cluster) observe(o *Object, typ obs.WatchType, initial bool) {
	rec := &obs.ObjectObservation{
		At: c.Now, Type: typ, Ref: o.Ref, Initial: initial, ResourceVersion: c.nextRV(),
		Generation: o.Generation, ObservedGeneration: o.ObsGen, CreatedAt: o.Created.Truncate(time.Second),
		Revision: o.Revision, TemplateHash: o.Hash,
	}
	o.LastRV = rec.ResourceVersion
	if o.Owner != nil {
		rec.Owners = []obs.OwnerRef{{Kind: o.Owner.Ref.Kind, Name: o.Owner.Ref.Name, UID: o.Owner.Ref.UID, Controller: true}}
	}
	switch o.Ref.Kind {
	case "Deployment", "ReplicaSet", "StatefulSet":
		rec.Replicas = i32(o.Replicas)
		rec.ReadyReplicas = i32(o.Ready)
		rec.StatusReplicas = i32(o.Ready)
		rec.UpdatedReplicas = i32(o.Updated)
		rec.AvailableReplicas = i32(o.Ready)
	case "Pod":
		rec.Ready = bptr(o.Ready > 0)
	}
	if o.Deleted && typ != obs.WatchDeleted {
		rec.DeletingAt = c.Now.Truncate(time.Second)
	}
	c.emit(obs.Record{Kind: obs.KindObject, Object: rec})
}

func (c *Cluster) event(o *Object, reason, note, controller string) {
	c.emit(obs.Record{Kind: obs.KindEvent, Event: &obs.EventObservation{
		UID: c.UID(), At: c.Now, Regarding: o.Ref, Reason: reason, Type: "Normal", Note: note, Controller: controller, Count: 1,
	}})
}

// Workload is a simulated Deployment with its ReplicaSets and Pods.
type Workload struct {
	c        *Cluster
	Deploy   *Object
	Current  *Object
	Old      []*Object
	Pods     map[*Object][]*Object
	revision int
}

// Sources reports all sources healthy since the current time.
func (c *Cluster) Sources() {
	for _, s := range []string{"kubernetes-audit", "kubernetes-watch", "kubernetes-events", "otlp"} {
		c.emit(obs.Record{Kind: obs.KindSource, Source: &obs.SourceStatus{Source: s, At: c.Now, Healthy: true, StartedAt: c.Now}})
	}
}

// Deployment creates a stable Deployment with one ReplicaSet and n ready
// Pods, observed through the initial list.
func (c *Cluster) Deployment(ns, name string, n int) *Workload {
	w := &Workload{c: c, Pods: map[*Object][]*Object{}}
	w.Deploy = &Object{Ref: model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: name, UID: c.UID()},
		Generation: 1, ObsGen: 1, Replicas: n, Ready: n, Updated: n, Revision: "1", Created: c.Now.Add(-time.Hour)}
	c.observe(w.Deploy, obs.WatchAdded, true)
	w.revision = 1
	rs := w.newRS()
	rs.Created = c.Now.Add(-time.Hour)
	rs.Replicas, rs.Ready, rs.Updated = n, n, n
	c.observe(rs, obs.WatchAdded, true)
	for range n {
		p := w.newPod(rs)
		p.Created = c.Now.Add(-time.Hour)
		p.Ready = 1
		c.observe(p, obs.WatchAdded, true)
	}
	return w
}

func (w *Workload) newRS() *Object {
	c := w.c
	hash := c.id("hash")[:10]
	rs := &Object{Ref: model.ObjectRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Namespace: w.Deploy.Ref.Namespace,
		Name: w.Deploy.Ref.Name + "-" + hash, UID: c.UID()}, Owner: w.Deploy, Generation: 1, ObsGen: 1,
		Revision: strconv.Itoa(w.revision), Hash: hash, Created: c.Now}
	if w.Current != nil {
		w.Old = append(w.Old, w.Current)
	}
	w.Current = rs
	return rs
}

func (w *Workload) newPod(rs *Object) *Object {
	c := w.c
	p := &Object{Ref: model.ObjectRef{APIVersion: "v1", Kind: "Pod", Namespace: rs.Ref.Namespace,
		Name: rs.Ref.Name + "-" + c.id("pod")[:5], UID: c.UID()}, Owner: rs, Created: c.Now, Hash: rs.Hash}
	w.Pods[rs] = append(w.Pods[rs], p)
	return p
}

// Request describes a simulated API request for an action.
type Request struct {
	AuditID string
	Verb    string
	Res     string
	Sub     string
	NS      string
	Name    string
	User    string
	At      time.Time
}

// Audit emits an audit record for a request.
func (c *Cluster) Audit(r Request) {
	c.emit(obs.Record{Kind: obs.KindAudit, Audit: &obs.AuditRequest{
		AuditID: r.AuditID, Verb: r.Verb, APIGroup: groupFor(r.Res), APIVersion: "v1", Resource: r.Res, Subresource: r.Sub,
		Namespace: r.NS, Name: r.Name, User: r.User, UserAgent: "synthetic/0", StatusCode: 200,
		ReceivedAt: r.At, CompletedAt: r.At.Add(5 * time.Millisecond),
	}})
}

func groupFor(res string) string {
	switch res {
	case "deployments", "replicasets", "statefulsets":
		return "apps"
	}
	return ""
}

// MCPOptions controls the simulated tool call.
type MCPOptions struct {
	// DropTraceLink omits the client span (uninstrumented Kubernetes client).
	DropTraceLink bool
	// DropAudit omits the audit record.
	DropAudit bool
	// User is the Kubernetes identity of the tool server.
	User string
}

// MCPCall emits the spans and audit record of an MCP tool call that issues
// one Kubernetes request against obj. It returns the trace ID. The object
// observation for the mutation must be emitted by the caller immediately
// after (see Workload methods), with the returned RV hook.
func (c *Cluster) MCPCall(tool string, req Request, obj *Object, opt MCPOptions) (traceID string, finish func()) {
	traceID = c.TraceID()
	agentSpan := c.SpanID()
	toolSpan := c.SpanID()
	clientSpan := c.SpanID()
	start := c.Now
	req.At = start.Add(3 * time.Millisecond)
	if req.AuditID == "" {
		req.AuditID = c.UID()
	}
	if opt.User == "" {
		opt.User = "system:serviceaccount:effecttrace-demo:demo-actor"
	}
	req.User = opt.User
	if !opt.DropAudit {
		c.Audit(req)
	}
	finish = func() {
		end := c.Now
		c.emit(obs.Record{Kind: obs.KindSpan, Span: &obs.Span{
			TraceID: traceID, SpanID: agentSpan, Name: "tools/call " + tool, Kind: obs.SpanKindClient,
			Service: "demo-agent", Start: start.Add(-2 * time.Millisecond), End: end.Add(2 * time.Millisecond),
			Attributes: map[string]string{semconv.MCPMethodName: semconv.MCPMethodToolsCall, semconv.GenAIToolName: tool},
		}})
		c.emit(obs.Record{Kind: obs.KindSpan, Span: &obs.Span{
			TraceID: traceID, SpanID: toolSpan, ParentSpanID: agentSpan, Name: "tools/call " + tool, Kind: obs.SpanKindServer,
			Service: "demo-tools", Start: start, End: end,
			Attributes: map[string]string{semconv.MCPMethodName: semconv.MCPMethodToolsCall, semconv.GenAIToolName: tool,
				semconv.GenAIOperationName: semconv.GenAIOperationExecuteTool},
		}})
		if opt.DropTraceLink {
			return
		}
		attrs := map[string]string{
			semconv.K8sVerb: req.Verb, semconv.K8sResource: req.Res, semconv.K8sObjectNamespace: req.NS,
			semconv.K8sObjectName: req.Name, semconv.HTTPResponseStatusCode: "200", semconv.K8sAuditID: req.AuditID,
		}
		if req.Sub != "" {
			attrs[semconv.K8sSubresource] = req.Sub
		}
		if g := groupFor(req.Res); g != "" {
			attrs[semconv.K8sAPIGroup] = g
		}
		if obj != nil && req.Verb != "delete" {
			attrs[semconv.K8sObjectUID] = obj.Ref.UID
			attrs[semconv.K8sObjectKind] = obj.Ref.Kind
			attrs[semconv.K8sObjectResourceVersion] = obj.LastRV
			if obj.Generation > 0 {
				attrs[semconv.K8sObjectGeneration] = strconv.FormatInt(obj.Generation, 10)
			}
		}
		c.emit(obs.Record{Kind: obs.KindSpan, Span: &obs.Span{
			TraceID: traceID, SpanID: clientSpan, ParentSpanID: toolSpan, Name: req.Verb + " " + req.Res, Kind: obs.SpanKindClient,
			Service: "demo-tools", Start: req.At.Add(-time.Millisecond), End: req.At.Add(8 * time.Millisecond), Attributes: attrs,
		}})
	}
	return traceID, finish
}

// Mutate records a direct spec change to the Deployment (generation bump).
func (w *Workload) Mutate() {
	w.c.Advance(10 * time.Millisecond)
	w.Deploy.Generation++
	w.c.observe(w.Deploy, obs.WatchModified, false)
}

// Rollout simulates the Deployment controller replacing all Pods with a new
// ReplicaSet. If fail is true, new Pods never become ready.
func (w *Workload) Rollout(fail bool) {
	c := w.c
	d := w.Deploy
	w.revision++
	old := w.Current
	c.Advance(40 * time.Millisecond)
	rs := w.newRS()
	c.observe(rs, obs.WatchAdded, false)
	d.Revision = strconv.Itoa(w.revision)
	d.ObsGen = d.Generation
	d.Updated = 0
	c.observe(d, obs.WatchModified, false)
	c.event(d, "ScalingReplicaSet", fmt.Sprintf("Scaled up replica set %s from 0 to %d", rs.Ref.Name, d.Replicas), "deployment-controller")
	n := d.Replicas
	for i := range n {
		c.Advance(150 * time.Millisecond)
		rs.Replicas = i + 1
		c.observe(rs, obs.WatchModified, false)
		p := w.newPod(rs)
		c.observe(p, obs.WatchAdded, false)
		c.event(rs, "SuccessfulCreate", "Created pod: "+p.Ref.Name, "replicaset-controller")
		if fail {
			continue
		}
		c.Advance(800 * time.Millisecond)
		p.Ready = 1
		c.observe(p, obs.WatchModified, false)
		rs.Ready = i + 1
		c.observe(rs, obs.WatchModified, false)
		// Scale down one old Pod.
		if pods := w.Pods[old]; len(pods) > 0 {
			op := pods[0]
			w.Pods[old] = pods[1:]
			old.Replicas--
			old.Ready--
			c.observe(old, obs.WatchModified, false)
			op.Deleted = true
			c.observe(op, obs.WatchModified, false)
			c.event(old, "SuccessfulDelete", "Deleted pod: "+op.Ref.Name, "replicaset-controller")
			c.Advance(300 * time.Millisecond)
			c.observe(op, obs.WatchDeleted, false)
		}
	}
	if fail {
		c.Advance(time.Second)
		return
	}
	c.Advance(100 * time.Millisecond)
	d.Updated, d.Ready = n, n
	c.observe(d, obs.WatchModified, false)
}

// Scale simulates the controllers scaling the current ReplicaSet to n.
func (w *Workload) Scale(n int) {
	c := w.c
	d := w.Deploy
	rs := w.Current
	from := d.Replicas
	d.Replicas = n
	c.Advance(30 * time.Millisecond)
	d.ObsGen = d.Generation
	c.observe(d, obs.WatchModified, false)
	rs.Replicas = n
	c.observe(rs, obs.WatchModified, false)
	verb := "up"
	if n < from {
		verb = "down"
	}
	c.event(d, "ScalingReplicaSet", fmt.Sprintf("Scaled %s replica set %s from %d to %d", verb, rs.Ref.Name, from, n), "deployment-controller")
	for i := from; i < n; i++ {
		c.Advance(100 * time.Millisecond)
		p := w.newPod(rs)
		c.observe(p, obs.WatchAdded, false)
		c.event(rs, "SuccessfulCreate", "Created pod: "+p.Ref.Name, "replicaset-controller")
	}
	for i := n; i < from; i++ {
		pods := w.Pods[rs]
		op := pods[len(pods)-1]
		w.Pods[rs] = pods[:len(pods)-1]
		op.Deleted = true
		c.Advance(50 * time.Millisecond)
		c.observe(op, obs.WatchModified, false)
		c.observe(op, obs.WatchDeleted, false)
	}
	c.Advance(900 * time.Millisecond)
	for _, p := range w.Pods[rs] {
		if p.Ready == 0 {
			p.Ready = 1
			c.observe(p, obs.WatchModified, false)
		}
	}
	rs.Ready, rs.Updated = n, n
	c.observe(rs, obs.WatchModified, false)
	d.Ready, d.Updated = n, n
	c.observe(d, obs.WatchModified, false)
}

// DeletePod simulates deleting one Pod of the current ReplicaSet and the
// ReplicaSet controller creating a replacement. It returns the deleted Pod.
func (w *Workload) DeletePod() *Object {
	c := w.c
	rs := w.Current
	pods := w.Pods[rs]
	op := pods[0]
	w.Pods[rs] = pods[1:]
	c.Advance(10 * time.Millisecond)
	op.Deleted = true
	c.observe(op, obs.WatchModified, false)
	c.Advance(60 * time.Millisecond)
	rs.Ready--
	c.observe(rs, obs.WatchModified, false)
	p := w.newPod(rs)
	c.observe(p, obs.WatchAdded, false)
	c.event(rs, "SuccessfulCreate", "Created pod: "+p.Ref.Name, "replicaset-controller")
	c.Advance(400 * time.Millisecond)
	c.observe(op, obs.WatchDeleted, false)
	c.Advance(600 * time.Millisecond)
	p.Ready = 1
	c.observe(p, obs.WatchModified, false)
	rs.Ready++
	c.observe(rs, obs.WatchModified, false)
	return op
}

// Pod returns the i-th live Pod of the current ReplicaSet.
func (w *Workload) Pod(i int) *Object { return w.Pods[w.Current][i] }

// Delete simulates deleting the Deployment with foreground garbage
// collection of its ReplicaSets and Pods.
func (w *Workload) Delete() {
	c := w.c
	var all []*Object
	for _, rs := range append(slices.Clone(w.Old), w.Current) {
		all = append(all, w.Pods[rs]...)
		all = append(all, rs)
	}
	all = append(all, w.Deploy)
	for _, o := range all {
		c.Advance(20 * time.Millisecond)
		o.Deleted = true
		c.observe(o, obs.WatchModified, false)
		c.observe(o, obs.WatchDeleted, false)
	}
}

// CreateDeployment simulates creating a Deployment while watched: the
// Deployment, its ReplicaSet and Pods are observed as new objects.
func (c *Cluster) CreateDeployment(ns, name string, n int) *Workload {
	w := &Workload{c: c, Pods: map[*Object][]*Object{}, revision: 1}
	w.Deploy = &Object{Ref: model.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: name, UID: c.UID()},
		Generation: 1, Replicas: n, Revision: "1", Created: c.Now}
	c.observe(w.Deploy, obs.WatchAdded, false)
	c.Advance(40 * time.Millisecond)
	rs := w.newRS()
	rs.Replicas = n
	c.observe(rs, obs.WatchAdded, false)
	for range n {
		c.Advance(80 * time.Millisecond)
		p := w.newPod(rs)
		c.observe(p, obs.WatchAdded, false)
	}
	c.Advance(time.Second)
	for _, p := range w.Pods[rs] {
		p.Ready = 1
		c.observe(p, obs.WatchModified, false)
	}
	rs.Ready, rs.Updated = n, n
	c.observe(rs, obs.WatchModified, false)
	w.Deploy.ObsGen = 1
	w.Deploy.Ready, w.Deploy.Updated = n, n
	c.observe(w.Deploy, obs.WatchModified, false)
	return w
}
