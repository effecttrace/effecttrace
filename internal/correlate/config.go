// Package correlate builds evidence-graded effect graphs from the observation
// store. Building is a pure function of the store contents and the
// configuration: the same observations always produce the same graph.
package correlate

import (
	"path"
	"strings"
	"time"
)

// Config controls action discovery and attribution windows. Every duration
// is documented in docs/evidence-model.md and docs/adr/004-temporal-effect-windows.md.
type Config struct {
	// SkewTolerance widens window starts to absorb clock and watch-delivery
	// skew between kube-apiserver timestamps and collector observation times.
	SkewTolerance time.Duration
	// MaxReconcile caps the reconciliation window when a workload never
	// reports a stable status (for example a failed rollout).
	MaxReconcile time.Duration
	// Settle extends a window after stabilization so that terminations and
	// Events emitted at the end of a rollout are observed.
	Settle time.Duration
	// BaselineWindow is the telemetry interval before the action used as the
	// comparison baseline.
	BaselineWindow time.Duration
	// TelemetrySettle extends the telemetry window after reconciliation.
	TelemetrySettle time.Duration
	// ControllerUsers are glob patterns of usernames whose mutations are
	// treated as reconciliation, not as actions.
	ControllerUsers []string
	// ActorUsers are glob patterns that override ControllerUsers, for
	// autonomous controllers whose decisions should be investigated as
	// actions (for example the HorizontalPodAutoscaler).
	ActorUsers []string
	// IgnoredResources are resources whose mutations are never actions.
	IgnoredResources []string
	// TemporalFallback enables TEMPORAL_CORRELATION edges from a tool call to
	// unlinked Kubernetes requests received during the tool span.
	TemporalFallback bool
	// MaxExclusions bounds the exclusion list per graph.
	MaxExclusions int
	// MaxDepth bounds ownership traversal.
	MaxDepth int
	// MaxTelemetryAge is how long after its telemetry window ends an action
	// may still be evaluated. A restarted collector does not re-evaluate
	// older actions; their graphs complete with telemetry reported as not
	// evaluated.
	MaxTelemetryAge time.Duration
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		SkewTolerance:   500 * time.Millisecond,
		MaxReconcile:    3 * time.Minute,
		Settle:          5 * time.Second,
		BaselineWindow:  60 * time.Second,
		TelemetrySettle: 15 * time.Second,
		ControllerUsers: []string{
			"system:kube-controller-manager",
			"system:kube-scheduler",
			"system:apiserver",
			"system:node:*",
			"system:serviceaccount:kube-system:*",
			"system:serviceaccount:local-path-storage:*",
		},
		ActorUsers: []string{
			"system:serviceaccount:kube-system:horizontal-pod-autoscaler",
		},
		IgnoredResources: []string{
			"events", "leases", "endpoints", "endpointslices",
			"tokenreviews", "subjectaccessreviews", "selfsubjectaccessreviews",
			"selfsubjectrulesreviews", "selfsubjectreviews", "localsubjectaccessreviews",
			"serviceaccounts/token", "certificatesigningrequests",
			"pods/exec", "pods/attach", "pods/portforward", "pods/proxy",
			"services/proxy", "nodes/proxy", "pods/status", "pods/binding", "pods/eviction",
			"deployments/status", "replicasets/status", "statefulsets/status", "jobs/status",
		},
		TemporalFallback: true,
		MaxExclusions:    50,
		MaxDepth:         4,
		MaxTelemetryAge:  10 * time.Minute,
	}
}

func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, s); err == nil && ok {
			return true
		}
		if p == s {
			return true
		}
	}
	return false
}

// IsControllerUser reports whether user's mutations are reconciliation.
func (c Config) IsControllerUser(user string) bool {
	if matchAny(c.ActorUsers, user) {
		return false
	}
	return matchAny(c.ControllerUsers, user)
}

// IsIgnoredResource reports whether mutations of resource/subresource are
// never treated as actions.
func (c Config) IsIgnoredResource(resource, subresource string) bool {
	full := resource
	if subresource != "" {
		full = resource + "/" + subresource
	}
	for _, r := range c.IgnoredResources {
		if r == full || (!strings.Contains(r, "/") && r == resource && subresource == "") {
			return true
		}
	}
	return false
}
