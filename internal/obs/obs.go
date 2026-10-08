// Package obs defines the normalized, privacy-minimized observations that
// EffectTrace sources produce and the correlation engine consumes. The same
// records are written by the recorder and read by replay, so an investigation
// can be rebuilt offline from exactly the data the collector saw.
package obs

import (
	"time"

	"github.com/effecttrace/effecttrace/pkg/model"
)

// Kind discriminates an observation record.
type Kind string

const (
	KindAudit  Kind = "audit"
	KindSpan   Kind = "span"
	KindObject Kind = "object"
	KindEvent  Kind = "event"
	KindMetric Kind = "metric"
	KindSource Kind = "source"
)

// Record is the envelope used on the ingest queue and in recordings.
// Exactly one payload field is set.
type Record struct {
	Kind   Kind               `json:"kind"`
	Audit  *AuditRequest      `json:"audit,omitempty"`
	Span   *Span              `json:"span,omitempty"`
	Object *ObjectObservation `json:"object,omitempty"`
	Event  *EventObservation  `json:"event,omitempty"`
	Metric *MetricResult      `json:"metric,omitempty"`
	Source *SourceStatus      `json:"source,omitempty"`
}

// AuditRequest is a kube-apiserver audit event reduced to request metadata.
// Request and response bodies are never retained.
type AuditRequest struct {
	AuditID     string    `json:"auditId"`
	Verb        string    `json:"verb"`
	APIGroup    string    `json:"apiGroup,omitempty"`
	APIVersion  string    `json:"apiVersion,omitempty"`
	Resource    string    `json:"resource"`
	Subresource string    `json:"subresource,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	Name        string    `json:"name,omitempty"`
	ObjectUID   string    `json:"objectUid,omitempty"`
	User        string    `json:"user"`
	UserAgent   string    `json:"userAgent,omitempty"`
	StatusCode  int       `json:"statusCode,omitempty"`
	DryRun      bool      `json:"dryRun,omitempty"`
	ReceivedAt  time.Time `json:"receivedAt"`
	CompletedAt time.Time `json:"completedAt"`
}

// SpanKind mirrors the OTLP span kind enumeration.
type SpanKind int

const (
	SpanKindUnspecified SpanKind = 0
	SpanKindInternal    SpanKind = 1
	SpanKindServer      SpanKind = 2
	SpanKindClient      SpanKind = 3
	SpanKindProducer    SpanKind = 4
	SpanKindConsumer    SpanKind = 5
)

// Span is an OpenTelemetry span reduced to identifiers, timing and an
// allowlisted attribute set.
type Span struct {
	TraceID      string            `json:"traceId"`
	SpanID       string            `json:"spanId"`
	ParentSpanID string            `json:"parentSpanId,omitempty"`
	Name         string            `json:"name"`
	Kind         SpanKind          `json:"kind"`
	Service      string            `json:"service,omitempty"`
	Start        time.Time         `json:"start"`
	End          time.Time         `json:"end"`
	Error        bool              `json:"error,omitempty"`
	Links        []model.TraceRef  `json:"links,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// OwnerRef is a reduced ownerReference.
type OwnerRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller,omitempty"`
}

// WatchType is the kind of watch notification.
type WatchType string

const (
	WatchAdded    WatchType = "ADDED"
	WatchModified WatchType = "MODIFIED"
	WatchDeleted  WatchType = "DELETED"
)

// ObjectObservation is one watch notification about a workload object,
// reduced to identity, ownership and rollout state. Specs, environment
// variables and data fields are never retained.
type ObjectObservation struct {
	// At is when the collector observed the notification.
	At   time.Time       `json:"at"`
	Type WatchType       `json:"type"`
	Ref  model.ObjectRef `json:"ref"`
	// Initial is true for objects delivered by the initial list.
	Initial            bool       `json:"initial,omitempty"`
	ResourceVersion    string     `json:"resourceVersion"`
	Generation         int64      `json:"generation,omitempty"`
	ObservedGeneration int64      `json:"observedGeneration,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"`
	DeletingAt         time.Time  `json:"deletingAt,omitzero"`
	Owners             []OwnerRef `json:"owners,omitempty"`
	Replicas           *int32     `json:"replicas,omitempty"`
	StatusReplicas     *int32     `json:"statusReplicas,omitempty"`
	ReadyReplicas      *int32     `json:"readyReplicas,omitempty"`
	UpdatedReplicas    *int32     `json:"updatedReplicas,omitempty"`
	AvailableReplicas  *int32     `json:"availableReplicas,omitempty"`
	Ready              *bool      `json:"ready,omitempty"`
	Revision           string     `json:"revision,omitempty"`
	TemplateHash       string     `json:"templateHash,omitempty"`
	Workload           string     `json:"workload,omitempty"`
}

// EventObservation is a Kubernetes Event reduced to its reference and reason.
type EventObservation struct {
	UID        string          `json:"uid"`
	At         time.Time       `json:"at"`
	Regarding  model.ObjectRef `json:"regarding"`
	Reason     string          `json:"reason"`
	Type       string          `json:"type,omitempty"`
	Note       string          `json:"note,omitempty"`
	Controller string          `json:"controller,omitempty"`
	Count      int32           `json:"count,omitempty"`
	// Initial is true for Events delivered by an informer's initial list,
	// whose time is the server's (second-precision) timestamp rather than
	// the collector's observation time.
	Initial bool `json:"initial,omitempty"`
}

// MetricResult is the evaluation of one telemetry signal for one workload
// over an action's observation window.
type MetricResult struct {
	ActionID      string    `json:"actionId"`
	Signal        string    `json:"signal"`
	Unit          string    `json:"unit,omitempty"`
	Namespace     string    `json:"namespace"`
	Workload      string    `json:"workload"`
	WorkloadUID   string    `json:"workloadUid,omitempty"`
	BaselineStart time.Time `json:"baselineStart"`
	WindowStart   time.Time `json:"windowStart"`
	WindowEnd     time.Time `json:"windowEnd"`
	Baseline      float64   `json:"baseline"`
	Observed      float64   `json:"observed"`
	Samples       int       `json:"samples"`
	Changed       bool      `json:"changed"`
	Direction     string    `json:"direction"`
	Error         string    `json:"error,omitempty"`
	// EvaluatedAt is when the collector ran the query. When a restarted
	// collector re-evaluates an action, the earliest evaluation is kept.
	EvaluatedAt time.Time `json:"evaluatedAt,omitzero"`
}

// SourceStatus reports source health so graphs can state their coverage.
type SourceStatus struct {
	Source    string    `json:"source"`
	At        time.Time `json:"at"`
	Healthy   bool      `json:"healthy"`
	Detail    string    `json:"detail,omitempty"`
	StartedAt time.Time `json:"startedAt,omitzero"`
}
