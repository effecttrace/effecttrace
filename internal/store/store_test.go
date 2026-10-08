package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/pkg/model"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func objRec(uid, rv string, at time.Time) obs.Record {
	return obs.Record{Kind: obs.KindObject, Object: &obs.ObjectObservation{At: at, Type: obs.WatchModified, ResourceVersion: rv,
		Ref: model.ObjectRef{Kind: "Pod", Namespace: "shop", Name: "p-" + uid, UID: uid}, CreatedAt: at}}
}

func TestApplyIsIdempotentAndOrdered(t *testing.T) {
	s := New(DefaultConfig())
	if ok, err := s.Apply(objRec("u", "2", t0.Add(2*time.Second))); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := s.Apply(objRec("u", "1", t0.Add(time.Second))); !ok {
		t.Fatal("out-of-order observation not applied")
	}
	if ok, _ := s.Apply(objRec("u", "2", t0.Add(2*time.Second))); ok {
		t.Fatal("replayed observation applied twice")
	}
	s.Read(func(v View) {
		h, _ := v.Object("u")
		if len(h.Observations) != 2 || h.Observations[0].ResourceVersion != "1" {
			t.Fatalf("observations not time-ordered: %+v", h.Observations)
		}
	})
	st := s.StatsSnapshot()
	if st.Duplicate[obs.KindObject] != 1 || st.Applied[obs.KindObject] != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestBoundsEvictOldest(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRequests, cfg.MaxObjects, cfg.MaxObservationsPerObj = 3, 2, 4
	s := New(cfg)
	for i := range 5 {
		s.Apply(obs.Record{Kind: obs.KindAudit, Audit: &obs.AuditRequest{AuditID: string(rune('a' + i)), Verb: "patch", Resource: "deployments", ReceivedAt: t0}})
		s.Apply(objRec(string(rune('a'+i)), "1", t0))
	}
	for i := range 10 {
		s.Apply(objRec("e", string(rune('0'+i)), t0.Add(time.Duration(i)*time.Second)))
	}
	s.Read(func(v View) {
		c := v.Counts()
		if c["requests"] != 3 || c["objects"] != 2 {
			t.Fatalf("counts = %v", c)
		}
		if _, ok := v.Request("a"); ok {
			t.Fatal("oldest request not evicted")
		}
		h, ok := v.Object("e")
		if !ok || len(h.Observations) != 4 || h.Dropped != 6 || h.Observations[0].ResourceVersion != "0" {
			t.Fatalf("per-object cap: %+v", h)
		}
	})
}

func TestValidateRejectsHostileInput(t *testing.T) {
	cases := map[string]obs.Record{
		"two payloads":  {Kind: obs.KindAudit, Audit: &obs.AuditRequest{}, Span: &obs.Span{}},
		"zero trace":    {Kind: obs.KindSpan, Span: &obs.Span{TraceID: strings.Repeat("0", 32), SpanID: "0123456789abcdef", Start: t0}},
		"upper hex":     {Kind: obs.KindSpan, Span: &obs.Span{TraceID: strings.Repeat("A", 32), SpanID: "0123456789abcdef", Start: t0}},
		"ansi name":     {Kind: obs.KindSpan, Span: &obs.Span{TraceID: strings.Repeat("a", 32), SpanID: "0123456789abcdef", Start: t0, Name: "x\x1b[2J"}},
		"huge attr":     {Kind: obs.KindSpan, Span: &obs.Span{TraceID: strings.Repeat("a", 32), SpanID: "0123456789abcdef", Start: t0, Attributes: map[string]string{"k": strings.Repeat("v", MaxAttrValueLen+1)}}},
		"end before":    {Kind: obs.KindSpan, Span: &obs.Span{TraceID: strings.Repeat("a", 32), SpanID: "0123456789abcdef", Start: t0, End: t0.Add(-time.Second)}},
		"missing uid":   {Kind: obs.KindObject, Object: &obs.ObjectObservation{At: t0, Type: obs.WatchAdded, ResourceVersion: "1", Ref: model.ObjectRef{Kind: "Pod", Name: "p"}}},
		"self owner":    {Kind: obs.KindObject, Object: &obs.ObjectObservation{At: t0, Type: obs.WatchAdded, ResourceVersion: "1", Ref: model.ObjectRef{Kind: "Pod", Name: "p", UID: "u"}, Owners: []obs.OwnerRef{{UID: "u", Kind: "Pod", Name: "p"}}}},
		"html name ok?": {Kind: obs.KindObject, Object: &obs.ObjectObservation{At: t0, Type: "EVIL", ResourceVersion: "1", Ref: model.ObjectRef{Kind: "Pod", Name: "<script>", UID: "u"}}},
		"year 1":        {Kind: obs.KindAudit, Audit: &obs.AuditRequest{AuditID: "x", Verb: "patch", Resource: "r"}},
		"unknown kind":  {Kind: "bogus", Source: &obs.SourceStatus{Source: "s"}},
	}
	for name, r := range cases {
		if err := Validate(r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

func TestPruneDropsExpiredDeletedObjects(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Retention = time.Minute
	s := New(cfg)
	r := objRec("old", "1", t0)
	r.Object.Type = obs.WatchDeleted
	s.Apply(r)
	s.Apply(objRec("live", "1", t0))
	s.Prune(t0.Add(2 * time.Minute))
	s.Read(func(v View) {
		if _, ok := v.Object("old"); ok {
			t.Error("expired deleted object retained")
		}
	})
}

func FuzzApplyRecordJSON(f *testing.F) {
	for _, r := range []obs.Record{
		objRec("u", "1", t0),
		{Kind: obs.KindAudit, Audit: &obs.AuditRequest{AuditID: "x", Verb: "patch", Resource: "deployments", ReceivedAt: t0}},
		{Kind: obs.KindEvent, Event: &obs.EventObservation{UID: "e", At: t0, Regarding: model.ObjectRef{UID: "u"}, Reason: "x"}},
	} {
		b, _ := json.Marshal(r)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var r obs.Record
		if json.Unmarshal(data, &r) != nil {
			return
		}
		s := New(DefaultConfig())
		ok1, err := s.Apply(r)
		if err != nil {
			return
		}
		ok2, err2 := s.Apply(r)
		if err2 != nil || (ok1 && ok2 && r.Kind != obs.KindSource) {
			t.Fatalf("replay not idempotent: %v %v %v", ok1, ok2, err2)
		}
		s.Read(func(v View) { _ = v.Requests(); _ = v.Spans(); _ = v.Counts() })
	})
}

func TestMergesAreOrderIndependent(t *testing.T) {
	// Duplicates seen across collector restarts must merge to the same state
	// whatever order they arrive in.
	relist := func(at time.Time) obs.Record {
		r := objRec("u", "7", at)
		r.Object.Type, r.Object.Initial = obs.WatchAdded, true
		return r
	}
	ev := func(at time.Time, count int32, note string) obs.Record {
		return obs.Record{Kind: obs.KindEvent, Event: &obs.EventObservation{UID: "e", At: at, Regarding: model.ObjectRef{UID: "u"}, Reason: "SuccessfulCreate", Note: note, Count: count}}
	}
	metric := func(samples int) obs.Record {
		return obs.Record{Kind: obs.KindMetric, Metric: &obs.MetricResult{ActionID: "a", Signal: "p99", Workload: "w", Samples: samples, Observed: float64(samples)}}
	}
	src := func(started time.Time) obs.Record {
		return obs.Record{Kind: obs.KindSource, Source: &obs.SourceStatus{Source: "kubernetes-watch", At: started, StartedAt: started, Healthy: true}}
	}
	recs := []obs.Record{relist(t0.Add(time.Hour)), relist(t0), ev(t0.Add(time.Minute), 3, "Created pod: b"), ev(t0, 1, "Created pod: a"),
		ev(t0.Add(2*time.Minute), 3, "Created pod: a"), metric(10), metric(52), src(t0.Add(time.Hour)), src(t0)}
	state := func(order []int) string {
		s := New(DefaultConfig())
		for _, i := range order {
			s.Apply(recs[i])
		}
		var out string
		s.Read(func(v View) {
			h, _ := v.Object("u")
			e := v.EventsRegarding("u")[0]
			m := v.Metrics("a")[0]
			st, _ := v.Source("kubernetes-watch")
			out = fmt.Sprintf("%v|%v %d %s|%d|%v %v", h.First().At, e.At, e.Count, e.Note, m.Samples, st.StartedAt, v.SourceInstances("kubernetes-watch"))
		})
		return out
	}
	want := state([]int{0, 1, 2, 3, 4, 5, 6, 7, 8})
	for _, order := range [][]int{{8, 7, 6, 5, 4, 3, 2, 1, 0}, {3, 6, 1, 8, 0, 4, 2, 7, 5}, {1, 0, 4, 2, 3, 5, 6, 8, 7}} {
		if got := state(order); got != want {
			t.Fatalf("order %v:\n got %s\nwant %s", order, got, want)
		}
	}
	if !strings.Contains(want, "Created pod: a") || !strings.Contains(want, "|52|") {
		t.Fatalf("unexpected merge result %s", want)
	}
}
