package telemetry

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/effecttrace/effecttrace/internal/correlate"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		base, obs, abs, rel float64
		changed             bool
		dir                 string
	}{
		{0.05, 0.15, 0.03, 0.5, true, "increased"},
		{0.05, 0.06, 0.03, 0.5, false, "unchanged"},
		{0.20, 0.05, 0.03, 0.5, true, "decreased"},
		{0, 0.01, 0.02, 1, false, "unchanged"},
	}
	for _, c := range cases {
		ch, d := Compare(c.base, c.obs, c.abs, c.rel)
		if ch != c.changed || d != c.dir {
			t.Errorf("Compare(%v,%v) = %v %s", c.base, c.obs, ch, d)
		}
	}
}

func TestEvaluateBaselineVersusWindow(t *testing.T) {
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		var vals []string
		for i := -30; i <= 30; i += 2 {
			v := "0.05"
			if i > 4 && i < 20 {
				v = "0.20"
			}
			vals = append(vals, fmt.Sprintf("[%d,%q]", start.Add(time.Duration(i)*time.Second).Unix(), v))
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[%s]}]}}`, strings.Join(vals, ","))
	}))
	defer srv.Close()
	e := &Evaluator{BaseURL: srv.URL, Signals: []Signal{{Name: "p99", Unit: "seconds", Query: `q{ns="$namespace",svc="$workload"}`, AbsThreshold: 0.03, RelThreshold: 0.5}}}
	res := e.Evaluate(context.Background(), correlate.TelemetryTarget{ActionID: "a", Namespace: "shop", BaselineStart: start.Add(-30 * time.Second),
		WindowStart: start, WindowEnd: start.Add(30 * time.Second), InScope: []correlate.Workload{{Name: "checkout", UID: "u"}}})
	if len(res) != 1 || !res[0].Changed || res[0].Direction != "increased" || math.Abs(res[0].Baseline-0.05) > 1e-9 || math.Abs(res[0].Observed-0.20) > 1e-9 {
		t.Fatalf("result %+v", res)
	}
	if gotQuery != `q{ns="shop",svc="checkout"}` {
		t.Errorf("query %q", gotQuery)
	}
}

func TestEvaluateRefusesInjection(t *testing.T) {
	e := &Evaluator{BaseURL: "http://127.0.0.1:1", Signals: []Signal{{Name: "x", Query: `q{svc="$workload"}`}}}
	res := e.Evaluate(context.Background(), correlate.TelemetryTarget{Namespace: "shop", InScope: []correlate.Workload{{Name: `a"} or vector(1) #`}}})
	if res[0].Error == "" {
		t.Fatal("non-DNS workload name was substituted into PromQL")
	}
}

func TestEvaluateReportsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"status":"error","error":"parse error"}`)
	}))
	defer srv.Close()
	e := &Evaluator{BaseURL: srv.URL, Signals: []Signal{{Name: "x", Query: `q{svc="$workload"}`}}}
	res := e.Evaluate(context.Background(), correlate.TelemetryTarget{Namespace: "shop", InScope: []correlate.Workload{{Name: "checkout"}}})
	if !strings.Contains(res[0].Error, "parse error") || res[0].Changed {
		t.Fatalf("result %+v", res[0])
	}
}

func TestSignalValidate(t *testing.T) {
	if (Signal{Name: "ok_name", Query: "x{$workload}"}).Validate() != nil {
		t.Error("valid signal rejected")
	}
	for _, s := range []Signal{{Name: "Bad-Name", Query: "$workload"}, {Name: "x", Query: "no placeholder"}, {Name: "x", Query: "$workload", Reduce: "sum"}} {
		if s.Validate() == nil {
			t.Errorf("invalid signal accepted: %+v", s)
		}
	}
}

func TestEvaluateFailsFastWhenUnreachable(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	url := srv.URL
	srv.Close() // nothing listens: every request fails at the transport
	e := &Evaluator{BaseURL: url, Signals: []Signal{{Name: "a", Query: `q{s="$workload"}`}, {Name: "b", Query: `q{s="$workload"}`}}}
	start := time.Now()
	res := e.Evaluate(context.Background(), correlate.TelemetryTarget{Namespace: "shop",
		InScope: []correlate.Workload{{Name: "checkout"}}, Others: []correlate.Workload{{Name: "payments"}, {Name: "inventory"}}})
	if len(res) != 6 {
		t.Fatalf("results = %d", len(res))
	}
	for _, r := range res {
		if r.Error == "" {
			t.Fatalf("missing error: %+v", r)
		}
	}
	if time.Since(start) > 3*time.Second || calls != 0 {
		t.Fatalf("did not fail fast (%v, %d calls)", time.Since(start), calls)
	}
}
