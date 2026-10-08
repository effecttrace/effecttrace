package model

import (
	"time"
)

// ActionKind classifies the initiating operation.
type ActionKind string

const (
	// ActionMCPToolCall is an MCP tools/call observed as an OpenTelemetry
	// server span.
	ActionMCPToolCall ActionKind = "MCP_TOOL_CALL"
	// ActionKubernetesAPICall is a mutating Kubernetes API request observed
	// in the kube-apiserver audit log that is not part of a traced action.
	ActionKubernetesAPICall ActionKind = "KUBERNETES_API_CALL"
)

// NodeType classifies a node in the effect graph.
type NodeType string

const (
	NodeAction            NodeType = "ACTION"
	NodeKubernetesRequest NodeType = "KUBERNETES_REQUEST"
	NodeKubernetesObject  NodeType = "KUBERNETES_OBJECT"
	NodeKubernetesEvent   NodeType = "KUBERNETES_EVENT"
	NodeTraceSpan         NodeType = "TRACE_SPAN"
	NodeMetricObservation NodeType = "METRIC_OBSERVATION"
)

// GraphStatus is the lifecycle state of an effect graph.
type GraphStatus string

const (
	// StatusObserving means the reconciliation window is still open.
	StatusObserving GraphStatus = "OBSERVING"
	// StatusSettling means the reconciliation window closed and telemetry
	// evaluation is pending.
	StatusSettling GraphStatus = "SETTLING"
	// StatusComplete means all windows closed and telemetry was evaluated.
	StatusComplete GraphStatus = "COMPLETE"
)

// ObjectRef identifies a Kubernetes object. UID is authoritative; names are
// descriptive and may be reused after deletion.
type ObjectRef struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
}

// TraceRef identifies an OpenTelemetry span.
type TraceRef struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
}

// Action is the initiating observed operation of an effect graph.
type Action struct {
	ID        string      `json:"id"`
	Kind      ActionKind  `json:"kind"`
	Name      string      `json:"name"`
	Tool      string      `json:"tool,omitempty"`
	Actor     string      `json:"actor,omitempty"`
	Service   string      `json:"service,omitempty"`
	StartedAt time.Time   `json:"startedAt"`
	EndedAt   time.Time   `json:"endedAt,omitzero"`
	Trace     *TraceRef   `json:"trace,omitempty"`
	AuditID   string      `json:"auditId,omitempty"`
	Outcome   string      `json:"outcome,omitempty"`
	Targets   []ObjectRef `json:"targets,omitempty"`
}

// ObjectChange describes what happened to an object inside the observation
// window.
type ObjectChange struct {
	// Type is one of MUTATED, CREATED, DELETED, SCALED, UNCHANGED.
	Type string `json:"type"`
	// GenerationFrom/To record metadata.generation around a mutation, when
	// known.
	GenerationFrom int64 `json:"generationFrom,omitempty"`
	GenerationTo   int64 `json:"generationTo,omitempty"`
	// ResourceVersion is the opaque resourceVersion produced by a direct
	// mutation, when known. It is compared only for equality.
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// ReplicasFrom/To record a desired replica change for scalable objects.
	ReplicasFrom *int32 `json:"replicasFrom,omitempty"`
	ReplicasTo   *int32 `json:"replicasTo,omitempty"`
	// Revision is the Deployment rollout revision annotation, when present.
	Revision string `json:"revision,omitempty"`
	// At is when the change was observed.
	At time.Time `json:"at,omitzero"`
	// ReadyAt is when a created Pod first reported Ready, if observed.
	ReadyAt time.Time `json:"readyAt,omitzero"`
	// DeletedAt is set when an object created in the window was also deleted
	// in the window (for example a Pod of a failed rollout).
	DeletedAt time.Time `json:"deletedAt,omitzero"`
}

// RequestInfo describes one Kubernetes API request.
type RequestInfo struct {
	Verb        string    `json:"verb"`
	Resource    string    `json:"resource"`
	Subresource string    `json:"subresource,omitempty"`
	APIGroup    string    `json:"apiGroup,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	Name        string    `json:"name,omitempty"`
	AuditID     string    `json:"auditId,omitempty"`
	User        string    `json:"user,omitempty"`
	UserAgent   string    `json:"userAgent,omitempty"`
	StatusCode  int       `json:"statusCode,omitempty"`
	ReceivedAt  time.Time `json:"receivedAt,omitzero"`
	// Sources lists which independent observations confirm the request:
	// "otlp-client-span", "kubernetes-audit".
	Sources []string `json:"sources"`
}

// EventInfo describes a Kubernetes Event.
type EventInfo struct {
	Reason     string `json:"reason"`
	Type       string `json:"type,omitempty"`
	Note       string `json:"note,omitempty"`
	Controller string `json:"controller,omitempty"`
	Count      int32  `json:"count,omitempty"`
}

// MetricInfo describes a telemetry observation evaluated over a window.
type MetricInfo struct {
	Signal    string  `json:"signal"`
	Unit      string  `json:"unit,omitempty"`
	Workload  string  `json:"workload"`
	Baseline  float64 `json:"baseline"`
	Observed  float64 `json:"observed"`
	Direction string  `json:"direction"`
	// Changed is true when the observed value differs from the baseline by
	// more than the configured threshold.
	Changed bool `json:"changed"`
	Samples int  `json:"samples"`
}

// Node is a vertex in an effect graph.
type Node struct {
	ID         string        `json:"id"`
	Type       NodeType      `json:"type"`
	Label      string        `json:"label"`
	Object     *ObjectRef    `json:"object,omitempty"`
	Change     *ObjectChange `json:"change,omitempty"`
	Request    *RequestInfo  `json:"request,omitempty"`
	Span       *TraceRef     `json:"span,omitempty"`
	Event      *EventInfo    `json:"event,omitempty"`
	Metric     *MetricInfo   `json:"metric,omitempty"`
	ObservedAt time.Time     `json:"observedAt,omitzero"`
	// Grade is the weakest evidence on the strongest path from the action.
	Grade PathGrade `json:"grade,omitempty"`
}

// Fact is one piece of evidence recorded on an edge, such as an identifier
// that matched in two independent sources.
type Fact struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Edge is an evidence-graded relationship between two nodes.
type Edge struct {
	ID           string       `json:"id"`
	From         string       `json:"from"`
	To           string       `json:"to"`
	Relationship Relationship `json:"relationship"`
	Evidence     EvidenceType `json:"evidence"`
	Reason       string       `json:"reason"`
	ObservedAt   time.Time    `json:"observedAt,omitzero"`
	// Source names the system the evidence came from: "otlp",
	// "kubernetes-audit", "kubernetes-watch", "kubernetes-events",
	// "prometheus".
	Source    string `json:"source"`
	SourceRef string `json:"sourceRef,omitempty"`
	Facts     []Fact `json:"facts,omitempty"`
	// Rule names the documented attribution rule that admitted the edge.
	Rule string `json:"rule,omitempty"`
}

// ObservationWindow is an interval in which effects were considered.
type ObservationWindow struct {
	Name   string    `json:"name"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Reason string    `json:"reason"`
	// Open is true while the window has not closed yet.
	Open bool `json:"open,omitempty"`
}

// Exclusion records an observation deliberately NOT attached to the graph,
// with the reason. Exclusions make false-attribution protection visible.
type Exclusion struct {
	Label     string     `json:"label"`
	Object    *ObjectRef `json:"object,omitempty"`
	Kind      string     `json:"kind"`
	At        time.Time  `json:"at,omitzero"`
	Reason    string     `json:"reason"`
	ClaimedBy string     `json:"claimedBy,omitempty"`
}

// Ambiguity records a change that falls in the scope of more than one action
// and is therefore not attributed to any of them.
type Ambiguity struct {
	Label      string     `json:"label"`
	Object     *ObjectRef `json:"object,omitempty"`
	At         time.Time  `json:"at,omitzero"`
	Candidates []string   `json:"candidates"`
	Reason     string     `json:"reason"`
}

// SourceCoverage reports whether a source was available for the window.
type SourceCoverage struct {
	Source    string `json:"source"`
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
}

// EffectGraph is one bounded investigation around an Action.
type EffectGraph struct {
	SchemaVersion string              `json:"schemaVersion"`
	ID            string              `json:"id"`
	Status        GraphStatus         `json:"status"`
	Action        Action              `json:"action"`
	Windows       []ObservationWindow `json:"windows"`
	Nodes         []Node              `json:"nodes"`
	Edges         []Edge              `json:"edges"`
	Ambiguities   []Ambiguity         `json:"ambiguities,omitempty"`
	Exclusions    []Exclusion         `json:"exclusions,omitempty"`
	Coverage      []SourceCoverage    `json:"coverage"`
	// Notes are human-readable caveats that apply to the whole graph.
	Notes []string `json:"notes,omitempty"`
}
