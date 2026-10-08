package correlate

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/pkg/model"
	"github.com/effecttrace/effecttrace/pkg/semconv"
)

// linkKind describes how a request was connected to its action.
type linkKind int

const (
	linkSelf     linkKind = iota // the action is the request itself
	linkTrace                    // client span descends from the tool span
	linkTemporal                 // request received during the tool span only
)

// request is one Kubernetes API request attributed to an action.
type request struct {
	nodeID   string
	link     linkKind
	audit    *obs.AuditRequest
	span     *obs.Span
	verb     string
	group    string
	resource string
	subres   string
	ns       string
	name     string
	kind     string
	at       time.Time
	// Values reported by the instrumented client from the response object.
	respUID string
	respRV  string
	respGen int64
	// traceDepth is the number of parent hops from the client span to the
	// tool span.
	traceDepth int
	// auditMismatch is set when the client span named an audit ID whose
	// audit event disagrees with the span on verb, resource or object.
	auditMismatch bool
	// auditUntrusted is set when the span's audit ID is shared by so many
	// requests that it is not used as evidence.
	auditUntrusted bool
}

// action is a discovered action with its requests.
type action struct {
	model.Action
	span     *obs.Span
	requests []*request
}

var resourceKinds = map[string]string{
	"deployments":              "Deployment",
	"replicasets":              "ReplicaSet",
	"statefulsets":             "StatefulSet",
	"daemonsets":               "DaemonSet",
	"jobs":                     "Job",
	"cronjobs":                 "CronJob",
	"pods":                     "Pod",
	"services":                 "Service",
	"configmaps":               "ConfigMap",
	"secrets":                  "Secret",
	"namespaces":               "Namespace",
	"horizontalpodautoscalers": "HorizontalPodAutoscaler",
	"persistentvolumeclaims":   "PersistentVolumeClaim",
	"ingresses":                "Ingress",
	"networkpolicies":          "NetworkPolicy",
	"serviceaccounts":          "ServiceAccount",
	"roles":                    "Role",
	"rolebindings":             "RoleBinding",
}

// KindForResource maps a resource name to its kind, falling back to the
// resource name itself for unknown resources.
func KindForResource(resource string) string {
	if k, ok := resourceKinds[resource]; ok {
		return k
	}
	return resource
}

func isMutating(verb string) bool {
	switch verb {
	case "create", "update", "patch", "delete", "deletecollection":
		return true
	}
	return false
}

func successful(code int) bool { return code >= 200 && code < 300 }

// discover returns all actions in a deterministic order (start time, ID).
func discover(v store.View, cfg Config) []*action {
	var actions []*action
	claimed := map[*obs.AuditRequest]bool{}

	spans := v.Spans()
	for _, sp := range spans {
		if sp.Kind != obs.SpanKindServer || sp.Attributes[semconv.MCPMethodName] != semconv.MCPMethodToolsCall {
			continue
		}
		a := mcpAction(v, sp)
		for _, r := range a.requests {
			if r.audit != nil {
				claimed[r.audit] = true
			}
		}
		actions = append(actions, a)
	}

	for _, r := range v.Requests() {
		if claimed[r] || !isActionRequest(r, cfg) {
			continue
		}
		actions = append(actions, apiAction(r, v.RequestID(r)))
	}

	if cfg.TemporalFallback {
		for _, a := range actions {
			if a.Kind != model.ActionMCPToolCall || len(a.requests) > 0 {
				continue
			}
			a.requests = temporalRequests(v, a, cfg, claimed)
		}
	}

	slices.SortFunc(actions, func(a, b *action) int {
		if c := a.StartedAt.Compare(b.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return actions
}

func isActionRequest(r *obs.AuditRequest, cfg Config) bool {
	return isMutating(r.Verb) && successful(r.StatusCode) && !r.DryRun &&
		!cfg.IsControllerUser(r.User) && !cfg.IsIgnoredResource(r.Resource, r.Subresource)
}

func mcpAction(v store.View, sp *obs.Span) *action {
	tool := sp.Attributes[semconv.GenAIToolName]
	a := &action{span: sp}
	a.ID = "mcp-" + sp.TraceID[:16] + "-" + sp.SpanID
	a.Kind = model.ActionMCPToolCall
	a.Name = sp.Name
	if a.Name == "" {
		a.Name = semconv.MCPMethodToolsCall + " " + tool
	}
	a.Tool = tool
	a.Service = sp.Service
	a.StartedAt = sp.Start
	a.EndedAt = sp.End
	a.Trace = &model.TraceRef{TraceID: sp.TraceID, SpanID: sp.SpanID}
	a.Outcome = "ok"
	if sp.Error || sp.Attributes[semconv.ErrorType] != "" {
		a.Outcome = "error"
	}

	trace := v.TraceSpans(sp.TraceID)
	byID := make(map[string]*obs.Span, len(trace))
	for _, s := range trace {
		byID[s.SpanID] = s
	}
	if sp.ParentSpanID != "" {
		if parent, ok := byID[sp.ParentSpanID]; ok && parent.Service != "" && parent.Service != sp.Service {
			a.Actor = "mcp-client:" + parent.Service
		}
	}
	for _, s := range trace {
		if s.SpanID == sp.SpanID || s.Kind != obs.SpanKindClient {
			continue
		}
		verb := s.Attributes[semconv.K8sVerb]
		if !isMutating(verb) {
			continue
		}
		depth, ok := descends(s, sp.SpanID, byID)
		if !ok {
			continue
		}
		r := spanRequest(v, s)
		r.link = linkTrace
		r.traceDepth = depth
		a.requests = append(a.requests, r)
	}
	slices.SortFunc(a.requests, func(x, y *request) int {
		if c := x.at.Compare(y.at); c != 0 {
			return c
		}
		return strings.Compare(x.nodeID, y.nodeID)
	})
	for _, r := range a.requests {
		if !successful(r.statusCode()) {
			continue
		}
		a.Targets = append(a.Targets, model.ObjectRef{Kind: r.kind, Namespace: r.ns, Name: r.name, UID: r.respUID})
	}
	return a
}

// descends walks parent links from s and reports whether ancestor is reached
// within a bounded number of hops.
func descends(s *obs.Span, ancestor string, byID map[string]*obs.Span) (int, bool) {
	cur := s
	for depth := 1; depth <= 32; depth++ {
		if cur.ParentSpanID == "" {
			return 0, false
		}
		if cur.ParentSpanID == ancestor {
			return depth, true
		}
		next, ok := byID[cur.ParentSpanID]
		if !ok {
			return 0, false
		}
		cur = next
	}
	return 0, false
}

func spanRequest(v store.View, s *obs.Span) *request {
	at := s.Attributes
	r := &request{
		span:     s,
		verb:     at[semconv.K8sVerb],
		group:    at[semconv.K8sAPIGroup],
		resource: at[semconv.K8sResource],
		subres:   at[semconv.K8sSubresource],
		ns:       at[semconv.K8sObjectNamespace],
		name:     at[semconv.K8sObjectName],
		at:       s.Start,
		respUID:  at[semconv.K8sObjectUID],
		respRV:   at[semconv.K8sObjectResourceVersion],
	}
	if g, err := strconv.ParseInt(at[semconv.K8sObjectGeneration], 10, 64); err == nil {
		r.respGen = g
	}
	r.kind = at[semconv.K8sObjectKind]
	if r.kind == "" || r.subres != "" {
		// A subresource response (for example autoscaling/v1 Scale) names
		// the subresource kind; the mutated object is the parent resource.
		r.kind = KindForResource(r.resource)
	}
	r.nodeID = "request:span:" + s.TraceID + ":" + s.SpanID
	if id := at[semconv.K8sAuditID]; id != "" {
		var match []*obs.AuditRequest
		cands := v.RequestsByAuditID(id)
		for _, a := range cands {
			if a.Verb == r.verb && a.Resource == r.resource && a.Namespace == r.ns && a.Name == r.name && a.Subresource == r.subres {
				match = append(match, a)
			}
		}
		switch {
		case v.AuditIDUntrusted(id):
			r.auditUntrusted = true
		case len(match) == 1:
			r.audit = match[0]
			r.at = match[0].ReceivedAt
			r.nodeID = "request:" + v.RequestID(match[0])
		case len(cands) > 0:
			// Disagreeing or reused audit IDs never confirm a request.
			r.auditMismatch = true
		}
	}
	return r
}

func auditRequest(a *obs.AuditRequest, requestID string) *request {
	return &request{
		nodeID:   "request:" + requestID,
		audit:    a,
		verb:     a.Verb,
		group:    a.APIGroup,
		resource: a.Resource,
		subres:   a.Subresource,
		ns:       a.Namespace,
		name:     a.Name,
		kind:     KindForResource(a.Resource),
		at:       a.ReceivedAt,
		respUID:  a.ObjectUID,
	}
}

func (r *request) statusCode() int {
	if r.audit != nil {
		return r.audit.StatusCode
	}
	if r.span != nil {
		if c, err := strconv.Atoi(r.span.Attributes[semconv.HTTPResponseStatusCode]); err == nil {
			return c
		}
	}
	// Unknown outcome: never treated as a successful mutation.
	return 0
}

func apiAction(r *obs.AuditRequest, requestID string) *action {
	req := auditRequest(r, requestID)
	req.link = linkSelf
	a := &action{requests: []*request{req}}
	a.ID = "k8s-" + requestID
	a.Kind = model.ActionKubernetesAPICall
	target := r.Resource
	if r.Subresource != "" {
		target += "/" + r.Subresource
	}
	obj := r.Name
	if r.Namespace != "" {
		obj = r.Namespace + "/" + r.Name
	}
	a.Name = strings.TrimSpace(r.Verb + " " + target + " " + obj)
	a.Actor = r.User
	a.Service = r.UserAgent
	a.StartedAt = r.ReceivedAt
	a.EndedAt = r.CompletedAt
	a.AuditID = r.AuditID
	a.Outcome = "ok"
	a.Targets = []model.ObjectRef{{Kind: req.kind, Namespace: r.Namespace, Name: r.Name, UID: r.ObjectUID}}
	return a
}

// temporalRequests returns unclaimed mutating requests received while the
// tool span was running. They are connected only by timing.
func temporalRequests(v store.View, a *action, cfg Config, claimed map[*obs.AuditRequest]bool) []*request {
	lo := a.StartedAt.Add(-cfg.SkewTolerance)
	hi := a.EndedAt.Add(cfg.SkewTolerance)
	var out []*request
	for _, r := range v.Requests() {
		if claimed[r] || !isMutating(r.Verb) || !successful(r.StatusCode) || r.DryRun {
			continue
		}
		if cfg.IsControllerUser(r.User) || cfg.IsIgnoredResource(r.Resource, r.Subresource) {
			continue
		}
		if r.ReceivedAt.Before(lo) || r.ReceivedAt.After(hi) {
			continue
		}
		req := auditRequest(r, v.RequestID(r))
		req.link = linkTemporal
		out = append(out, req)
	}
	return out
}
