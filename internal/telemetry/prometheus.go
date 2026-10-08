// Package telemetry evaluates configured Prometheus signals over an action's
// observation window and compares them with a baseline before the action.
//
// A change detected here only ever becomes a TEMPORAL_CORRELATION edge.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/privacy"
)

// Signal is a PromQL template evaluated per workload. The placeholders
// $namespace and $workload are substituted with validated Kubernetes names.
type Signal struct {
	Name         string  `json:"name"`
	Unit         string  `json:"unit,omitempty"`
	Query        string  `json:"query"`
	AbsThreshold float64 `json:"absThreshold"`
	RelThreshold float64 `json:"relThreshold"`
	// Reduce selects how window samples are summarized: "max" (default) or
	// "mean". Baselines always use the mean.
	Reduce string `json:"reduce,omitempty"`
}

// Validate checks a signal definition.
func (s Signal) Validate() error {
	if !nameRE.MatchString(s.Name) || len(s.Name) > 63 {
		return fmt.Errorf("invalid signal name %q", s.Name)
	}
	if !strings.Contains(s.Query, "$workload") {
		return fmt.Errorf("signal %s: query must reference $workload", s.Name)
	}
	if s.AbsThreshold < 0 || s.RelThreshold < 0 {
		return fmt.Errorf("signal %s: thresholds must be non-negative", s.Name)
	}
	switch s.Reduce {
	case "", "max", "mean":
	default:
		return fmt.Errorf("signal %s: reduce must be max or mean", s.Name)
	}
	return nil
}

var (
	nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// dnsRE accepts DNS-1123 subdomain names, the only values substituted
	// into queries; anything else is refused to prevent PromQL injection.
	dnsRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// Evaluator queries a Prometheus-compatible HTTP API.
type Evaluator struct {
	BaseURL string
	Client  *http.Client
	Signals []Signal
	Step    time.Duration
}

// MaxResponseBytes bounds a query response.
const MaxResponseBytes = 8 << 20

// Evaluate computes results for every signal and workload of a target.
// Workloads outside the action's scope are evaluated too so that changes
// there can be reported as exclusions.
func (e *Evaluator) Evaluate(ctx context.Context, t correlate.TelemetryTarget) []obs.MetricResult {
	var out []obs.MetricResult
	workloads := append(append([]correlate.Workload{}, t.InScope...), t.Others...)
	for _, w := range workloads {
		for _, s := range e.Signals {
			out = append(out, e.evaluate(ctx, t, w, s))
		}
	}
	if len(out) == 0 {
		// Record that evaluation happened so the graph can complete.
		out = append(out, obs.MetricResult{ActionID: t.ActionID, Signal: "none", Namespace: t.Namespace, Workload: "none",
			BaselineStart: t.BaselineStart, WindowStart: t.WindowStart, WindowEnd: t.WindowEnd, Error: "no signals or workloads to evaluate"})
	}
	return out
}

func (e *Evaluator) evaluate(ctx context.Context, t correlate.TelemetryTarget, w correlate.Workload, s Signal) obs.MetricResult {
	r := obs.MetricResult{
		ActionID: t.ActionID, Signal: s.Name, Unit: s.Unit, Namespace: t.Namespace, Workload: w.Name, WorkloadUID: w.UID,
		BaselineStart: t.BaselineStart, WindowStart: t.WindowStart, WindowEnd: t.WindowEnd, Direction: "unchanged",
		EvaluatedAt: time.Now().UTC(),
	}
	if !dnsRE.MatchString(t.Namespace) || !dnsRE.MatchString(w.Name) {
		r.Error = "workload or namespace name is not a valid DNS-1123 name"
		return r
	}
	q := strings.NewReplacer("$namespace", t.Namespace, "$workload", w.Name).Replace(s.Query)
	samples, err := e.queryRange(ctx, q, t.BaselineStart, t.WindowEnd)
	if err != nil {
		r.Error = privacy.Text(err.Error(), 200)
		return r
	}
	var base, win []float64
	for _, sm := range samples {
		switch {
		case sm.t.Before(t.WindowStart):
			base = append(base, sm.v)
		case !sm.t.After(t.WindowEnd):
			win = append(win, sm.v)
		}
	}
	r.Samples = len(win)
	if len(base) == 0 || len(win) == 0 {
		return r
	}
	r.Baseline = mean(base)
	if s.Reduce == "mean" {
		r.Observed = mean(win)
	} else {
		r.Observed = maxOf(win)
	}
	r.Changed, r.Direction = Compare(r.Baseline, r.Observed, s.AbsThreshold, s.RelThreshold)
	return r
}

// Compare reports whether observed differs from baseline by more than both
// the absolute threshold and the relative threshold times the baseline.
func Compare(baseline, observed, abs, rel float64) (bool, string) {
	diff := observed - baseline
	limit := math.Max(abs, rel*math.Abs(baseline))
	if math.Abs(diff) <= limit {
		return false, "unchanged"
	}
	if diff > 0 {
		return true, "increased"
	}
	return true, "decreased"
}

func mean(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v[1:] {
		m = math.Max(m, x)
	}
	return m
}

type sample struct {
	t time.Time
	v float64
}

type rangeResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (e *Evaluator) queryRange(ctx context.Context, q string, start, end time.Time) ([]sample, error) {
	step := e.Step
	if step <= 0 {
		step = 2 * time.Second
	}
	u, err := url.Parse(strings.TrimRight(e.BaseURL, "/") + "/api/v1/query_range")
	if err != nil {
		return nil, err
	}
	v := url.Values{}
	v.Set("query", q)
	v.Set("start", strconv.FormatFloat(float64(start.UnixMilli())/1000, 'f', 3, 64))
	v.Set("end", strconv.FormatFloat(float64(end.UnixMilli())/1000, 'f', 3, 64))
	v.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	u.RawQuery = v.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus query failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxResponseBytes {
		return nil, errors.New("prometheus response too large")
	}
	var rr rangeResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		return nil, fmt.Errorf("prometheus returned HTTP %d with an undecodable body", resp.StatusCode)
	}
	if rr.Status != "success" {
		return nil, fmt.Errorf("prometheus error: %s", rr.Error)
	}
	if rr.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("unexpected result type %q", rr.Data.ResultType)
	}
	if len(rr.Data.Result) > 1 {
		return nil, fmt.Errorf("query returned %d series; signals must aggregate to one series", len(rr.Data.Result))
	}
	var out []sample
	for _, series := range rr.Data.Result {
		for _, pair := range series.Values {
			var ts float64
			var vs string
			if json.Unmarshal(pair[0], &ts) != nil || json.Unmarshal(pair[1], &vs) != nil {
				continue
			}
			f, err := strconv.ParseFloat(vs, 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				continue
			}
			sec, frac := math.Modf(ts)
			out = append(out, sample{t: time.Unix(int64(sec), int64(frac*1e9)).UTC(), v: f})
		}
	}
	return out, nil
}
