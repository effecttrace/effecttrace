package experiments

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

// ResultsFile is test-results/results.json.
type ResultsFile struct {
	SchemaVersion string    `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Commit        string    `json:"commit"`
	Dirty         bool      `json:"dirty"`
	Scenarios     []*Result `json:"scenarios"`
}

// WriteJSON writes v as indented JSON.
func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Git returns the commit SHA and whether the tree has uncommitted changes.
func Git(repo string) (string, bool) {
	sha, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		return "uncommitted", true
	}
	st, _ := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=no").Output()
	return strings.TrimSpace(string(sha)), len(strings.TrimSpace(string(st))) > 0
}

// Stat summarizes a distribution.
type Stat struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

func stat(v []float64) *Stat {
	if len(v) == 0 {
		return nil
	}
	s := slices.Clone(v)
	sort.Float64s(s)
	q := func(p float64) float64 { return s[int(math.Ceil(p*float64(len(s))))-1] }
	return &Stat{N: len(s), P50: round(q(0.5)), P95: round(q(0.95)), Max: round(s[len(s)-1])}
}

func round(v float64) float64 { return math.Round(v*1000) / 1000 }

// Rate is a ratio with its counts.
type Rate struct {
	Counts
	Precision *float64 `json:"precision"`
	Recall    *float64 `json:"recall"`
}

func rate(c Counts) Rate {
	r := Rate{Counts: c, Precision: c.Precision(), Recall: c.Recall()}
	if r.Precision != nil {
		v := round(*r.Precision)
		r.Precision = &v
	}
	if r.Recall != nil {
		v := round(*r.Recall)
		r.Recall = &v
	}
	return r
}

// Summary is test-results/summary.json.
type Summary struct {
	SchemaVersion string                    `json:"schemaVersion"`
	GeneratedAt   time.Time                 `json:"generatedAt"`
	Commit        string                    `json:"commit"`
	Dirty         bool                      `json:"dirty"`
	Scenarios     map[string]int            `json:"scenarios"`
	ByCategory    map[string]map[string]int `json:"byCategory"`
	Attribution   struct {
		Scope                string   `json:"scope"`
		Actions              int      `json:"actions"`
		Direct               Rate     `json:"direct"`
		Structural           Rate     `json:"structural"`
		FalseAttachments     int      `json:"falseAttachments"`
		UnrelatedTelemetry   int      `json:"unrelatedTelemetryAttachments"`
		AmbiguousClaims      int      `json:"ambiguousClaims"`
		AmbiguitiesReported  int      `json:"ambiguitiesReported"`
		ConcurrentScenarios  int      `json:"concurrentScenarios"`
		ConcurrentSeparated  int      `json:"concurrentSeparated"`
		ConcurrentSeparation *float64 `json:"concurrentSeparationAccuracy"`
	} `json:"attribution"`
	Operational struct {
		FirstEffectSeconds *Stat  `json:"firstStructuralEffectSeconds"`
		CompleteSeconds    *Stat  `json:"graphCompleteSeconds"`
		QueryMilliseconds  *Stat  `json:"graphQueryMilliseconds"`
		Note               string `json:"note"`
	} `json:"operational"`
	Failed []string `json:"failed"`
}

// Summarize derives the summary from results only.
func Summarize(rf *ResultsFile) Summary {
	var s Summary
	s.SchemaVersion = "effecttrace.io/summary/v1"
	s.GeneratedAt, s.Commit, s.Dirty = rf.GeneratedAt, rf.Commit, rf.Dirty
	s.Scenarios = map[string]int{"total": 0, "pass": 0, "fail": 0, "error": 0, "unsupported": 0}
	s.ByCategory = map[string]map[string]int{}
	s.Failed = []string{}
	var direct, structural Counts
	var first, complete, query []float64
	s.Attribution.Scope = "live kind experiments (L*); replay variants with altered windows (R05, R06) are excluded because they change the configuration on purpose"
	for _, r := range rf.Scenarios {
		s.Scenarios["total"]++
		s.Scenarios[r.Status]++
		if s.ByCategory[r.Category] == nil {
			s.ByCategory[r.Category] = map[string]int{}
		}
		s.ByCategory[r.Category][r.Status]++
		if r.Status == "fail" || r.Status == "error" {
			s.Failed = append(s.Failed, r.ID+" "+r.Title)
		}
		if r.Category != "live" {
			continue
		}
		if r.Concurrent && r.Separated != nil {
			s.Attribution.ConcurrentScenarios++
			if *r.Separated {
				s.Attribution.ConcurrentSeparated++
			}
		}
		for _, o := range r.Actions {
			s.Attribution.Actions++
			direct.Add(o.Eval.Direct)
			structural.Add(o.Eval.Structural)
			s.Attribution.FalseAttachments += len(o.Eval.FalseAttachments)
			s.Attribution.UnrelatedTelemetry += len(o.Eval.UnrelatedTelemetry)
			s.Attribution.AmbiguousClaims += len(o.Eval.AmbiguousClaimed)
			s.Attribution.AmbiguitiesReported += o.Eval.Ambiguities
			if o.FirstEffectS > 0 {
				first = append(first, o.FirstEffectS)
			}
			if o.CompleteS > 0 {
				complete = append(complete, o.CompleteS)
			}
			query = append(query, o.QueryMS...)
		}
	}
	s.Attribution.Direct = rate(direct)
	s.Attribution.Structural = rate(structural)
	if n := s.Attribution.ConcurrentScenarios; n > 0 {
		v := round(float64(s.Attribution.ConcurrentSeparated) / float64(n))
		s.Attribution.ConcurrentSeparation = &v
	}
	s.Operational.FirstEffectSeconds = stat(first)
	s.Operational.CompleteSeconds = stat(complete)
	s.Operational.QueryMilliseconds = stat(query)
	s.Operational.Note = "Measured on a single-node kind cluster on a laptop by polling the API once per second; 'complete' includes the configured settle and telemetry windows. Not a production-scale measurement."
	return s
}

// Environment is test-results/environment.json.
type Environment struct {
	SchemaVersion string            `json:"schemaVersion"`
	CapturedAt    time.Time         `json:"capturedAt"`
	Commit        string            `json:"commit"`
	Components    map[string]string `json:"components"`
	Host          map[string]string `json:"host"`
}

func cmdOut(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// CaptureEnvironment records component versions. Image versions come from
// the lab manifests; the Kubernetes server version is queried live.
func CaptureEnvironment(repo, kubeServerVersion, engine string) Environment {
	commit, _ := Git(repo)
	env := Environment{SchemaVersion: "effecttrace.io/environment/v1", CapturedAt: time.Now().UTC(), Commit: commit,
		Components: map[string]string{
			"kubernetes":             kubeServerVersion,
			"kind":                   cmdOut(repo+"/.bin/kind", "version"),
			"kindNodeImage":          "kindest/node:v1.37.0",
			"opentelemetryCollector": "otel/opentelemetry-collector:0.162.0 (core distribution)",
			"prometheus":             "prom/prometheus:v3.15.0",
			"opentelemetryGo":        "go.opentelemetry.io/otel v1.47.0",
			"clientGo":               "k8s.io/client-go v0.37.1",
			"mcpGoSDK":               "github.com/modelcontextprotocol/go-sdk v1.8.0",
			"mcpProtocol":            "2026-07-28 (negotiated by the SDK)",
			"go":                     runtime.Version(),
			"containerEngine":        engine,
		},
		Host: map[string]string{
			"os":   runtime.GOOS,
			"arch": runtime.GOARCH,
			"cpus": fmt.Sprint(runtime.NumCPU()),
		},
	}
	return env
}
