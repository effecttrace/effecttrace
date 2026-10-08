package experiments

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"time"

	"github.com/effecttrace/effecttrace/internal/correlate"
	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/pipeline"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/pkg/model"
)

// LabConfig is the correlation configuration of the lab collector
// (deploy/collector/collector.yaml).
func LabConfig() correlate.Config {
	c := correlate.DefaultConfig()
	c.MaxReconcile = 90 * time.Second
	return c
}

type replay struct {
	recs []obs.Record
	bad  int
}

func loadRecording(repo string) (*replay, error) {
	f, err := os.Open(repo + "/.lab/shared/recording.jsonl")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, bad, err := pipeline.ReadRecording(f)
	if err != nil {
		return nil, err
	}
	return &replay{recs: recs, bad: bad}, nil
}

func engineOf(recs []obs.Record, cfg correlate.Config) (*correlate.Engine, time.Time) {
	st := store.New(store.DefaultConfig())
	tel := false
	for _, r := range recs {
		_, _ = st.Apply(r)
		if r.Kind == obs.KindMetric {
			tel = true
		}
	}
	var newest time.Time
	st.Read(func(v store.View) { newest = v.Newest() })
	return correlate.NewEngine(st, cfg, correlate.Options{TelemetryConfigured: tel}), newest.Add(time.Hour)
}

// structure returns the sorted node and edge IDs of a graph.
func structure(g *model.EffectGraph) ([]string, []string) {
	var n, e []string
	for _, x := range g.Nodes {
		n = append(n, x.ID)
	}
	for _, x := range g.Edges {
		e = append(e, x.ID)
	}
	slices.Sort(n)
	slices.Sort(e)
	return n, e
}

func allGraphs(eng *correlate.Engine, now time.Time) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, a := range eng.Actions(now, time.Time{}) {
		g, err := eng.Graph(now, a.ID)
		if err != nil {
			return nil, err
		}
		b, err := g.MarshalCanonical()
		if err != nil {
			return nil, err
		}
		out[a.ID] = b
	}
	return out, nil
}

func sameGraphs(a, b map[string][]byte) (bool, string) {
	if len(a) != len(b) {
		return false, fmt.Sprintf("%d vs %d graphs", len(a), len(b))
	}
	for id, x := range a {
		if !bytes.Equal(x, b[id]) {
			return false, "graph " + id + " differs"
		}
	}
	return true, fmt.Sprintf("%d graphs identical", len(a))
}

// priorOutcome returns the first action of a prior live scenario.
func priorOutcome(e *Env, id string) (*Outcome, error) {
	r := e.Prior[id]
	if r == nil || r.Status != "pass" && r.Status != "fail" || len(r.Actions) == 0 {
		return nil, fmt.Errorf("live scenario %s has no evaluated actions", id)
	}
	return r.Actions[0], nil
}

// ReplayScenarios returns offline experiments over the lab recording.
func ReplayScenarios() []Scenario {
	return []Scenario{
		{ID: "R01", Title: "Replay reproduces live graphs", Category: "replay",
			Description: "The collector's recording of the whole lab run is replayed offline. Every live graph (except those affected by a collector restart or a disabled source, where the recording legitimately contains more than the live collector saw) must have the same nodes and edges.",
			Run: func(_ context.Context, e *Env, r *Result) error {
				rp, err := loadRecording(e.Lab.Repo)
				if err != nil {
					return err
				}
				eng, now := engineOf(rp.recs, LabConfig())
				compared, equal := 0, 0
				var diffs []string
				for _, id := range sortedKeys(e.Prior) {
					if id == "L25" || id == "L27" || id == "L30" {
						continue
					}
					for _, o := range e.Prior[id].Actions {
						if o.Graph == nil {
							continue
						}
						g, err := eng.Graph(now, o.ActionID)
						if err != nil {
							diffs = append(diffs, o.ActionID+": missing")
							compared++
							continue
						}
						ln, le := structure(o.Graph)
						rn, re := structure(g)
						compared++
						if slices.Equal(ln, rn) && slices.Equal(le, re) {
							equal++
						} else {
							diffs = append(diffs, id+"/"+o.Label)
						}
					}
				}
				r.check("replayed graphs match live graphs", compared > 0 && equal == compared, "%d of %d graphs identical; differing: %v", equal, compared, diffs)
				r.note("Recording: %d records, %d unreadable.", len(rp.recs), rp.bad)
				return nil
			}},
		{ID: "R02", Title: "Out-of-order and duplicated input", Category: "replay",
			Description: "The recording is shuffled and a third of it is replayed twice. Ingest is idempotent and graphs are built from sorted state, so every graph must be byte-identical to the in-order replay.",
			Run: func(_ context.Context, e *Env, r *Result) error {
				rp, err := loadRecording(e.Lab.Repo)
				if err != nil {
					return err
				}
				base, now := engineOf(rp.recs, LabConfig())
				want, err := allGraphs(base, now)
				if err != nil {
					return err
				}
				shuffled := slices.Clone(rp.recs)
				rng := rand.New(rand.NewPCG(42, 7))
				rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
				shuffled = append(shuffled, shuffled[:len(shuffled)/3]...)
				eng, _ := engineOf(shuffled, LabConfig())
				got, err := allGraphs(eng, now)
				if err != nil {
					return err
				}
				ok, detail := sameGraphs(want, got)
				r.check("graphs identical under reordering and duplication", ok, "%s", detail)
				return nil
			}},
		{ID: "R03", Title: "Delayed audit stream", Category: "replay",
			Description: "Audit records are withheld and delivered last. Before they arrive, the L01 graph must still hold DIRECT evidence from the instrumented client and report audit coverage as missing; after they arrive it must equal the undelayed graph.",
			Run: delayed(obs.KindAudit, func(r *Result, partial *model.EffectGraph) {
				r.check("DIRECT evidence present before audit arrives", hasEdge(partial, model.EvidenceDirect, "Deployment"), "client response metadata")
				c := coverage(partial, "kubernetes-audit")
				r.check("audit coverage missing before audit arrives", !c.Available, "%s", c.Detail)
			})},
		{ID: "R04", Title: "Delayed Kubernetes Events", Category: "replay",
			Description: "Event records are withheld and delivered last. Before they arrive the L01 graph has no EVENT_REFERENCE edges but the same structural edges; after they arrive it equals the undelayed graph.",
			Run: delayed(obs.KindEvent, func(r *Result, partial *model.EffectGraph) {
				n := 0
				for _, ed := range partial.Edges {
					if ed.Evidence == model.EvidenceEventReference {
						n++
					}
				}
				r.check("no EVENT_REFERENCE edges before events arrive", n == 0, "%d", n)
				r.check("structural edges present before events arrive", hasEdge(partial, model.EvidenceStructural, "ReplicaSet"), "ReplicaSet edge")
			})},
		{ID: "R05", Title: "Observation window too short", Category: "replay",
			Description: "L01 is replayed with a 1 s maximum window and no settle period. Effects after the window are missed (lower recall), but nothing unrelated may be attached.",
			Run:         windowVariant(func(c *correlate.Config) { c.MaxReconcile = time.Second; c.Settle = 0 }, "L01", true)},
		{ID: "R06", Title: "Observation window very long", Category: "replay",
			Description: "L01, L02 and L03 (consecutive actions on checkout) are replayed with a 20 min maximum and 10 min settle period, so windows overlap later actions. Overlaps must turn into ambiguities, never into false attachments.",
			Run:         windowVariant(func(c *correlate.Config) { c.MaxReconcile = 20 * time.Minute; c.Settle = 10 * time.Minute }, "L01,L02,L03", false)},
		{ID: "R07", Title: "Corrupt recording input", Category: "replay",
			Description: "Garbage lines, truncated JSON, invalid UTF-8 and records that fail validation are mixed into the recording. They must be skipped and every graph must be identical to the clean replay.",
			Run: func(_ context.Context, e *Env, r *Result) error {
				raw, err := os.ReadFile(e.Lab.Repo + "/.lab/shared/recording.jsonl")
				if err != nil {
					return err
				}
				lines := bytes.Split(raw, []byte("\n"))
				junk := [][]byte{
					[]byte("not json at all"), []byte(`{"kind":"span","span":{"traceId":"`), {0xff, 0xfe, 0xfd},
					[]byte(`{"kind":"object","object":{"ref":{"kind":"Pod","name":"x","uid":"u"},"type":"ADDED","resourceVersion":"1","at":"0001-01-01T00:00:00Z"}}`),
					[]byte(`{"kind":"audit","audit":{"auditId":"\u001b[2J","verb":"patch","resource":"pods","receivedAt":"2026-10-01T00:00:00Z"}}`),
				}
				var mixed [][]byte
				for i, l := range lines {
					mixed = append(mixed, l)
					if i%500 == 0 {
						mixed = append(mixed, junk...)
					}
				}
				recs, bad, err := pipeline.ReadRecording(bytes.NewReader(bytes.Join(mixed, []byte("\n"))))
				if err != nil {
					return err
				}
				rp, err := loadRecording(e.Lab.Repo)
				if err != nil {
					return err
				}
				base, now := engineOf(rp.recs, LabConfig())
				want, err := allGraphs(base, now)
				if err != nil {
					return err
				}
				eng, _ := engineOf(recs, LabConfig())
				got, err := allGraphs(eng, now)
				if err != nil {
					return err
				}
				r.check("corrupt lines skipped", bad >= len(junk), "%d lines skipped", bad)
				ok, detail := sameGraphs(want, got)
				r.check("graphs unaffected by corrupt input", ok, "%s", detail)
				return nil
			}},
	}
}

func delayed(kind obs.Kind, partialChecks func(*Result, *model.EffectGraph)) func(context.Context, *Env, *Result) error {
	return func(_ context.Context, e *Env, r *Result) error {
		o, err := priorOutcome(e, "L01")
		if err != nil {
			return err
		}
		rp, err := loadRecording(e.Lab.Repo)
		if err != nil {
			return err
		}
		base, now := engineOf(rp.recs, LabConfig())
		want, err := base.Graph(now, o.ActionID)
		if err != nil {
			return err
		}
		var without, held []obs.Record
		for _, rec := range rp.recs {
			if rec.Kind == kind {
				held = append(held, rec)
			} else {
				without = append(without, rec)
			}
		}
		pe, _ := engineOf(without, LabConfig())
		partial, err := pe.Graph(now, o.ActionID)
		if err != nil {
			return err
		}
		partialChecks(r, partial)
		fe, _ := engineOf(append(without, held...), LabConfig())
		final, err := fe.Graph(now, o.ActionID)
		if err != nil {
			return err
		}
		a, _ := want.MarshalCanonical()
		b, _ := final.MarshalCanonical()
		r.check("graph converges once delayed records arrive", bytes.Equal(a, b), "canonical JSON compared")
		return nil
	}
}

func windowVariant(mut func(*correlate.Config), ids string, expectRecallDrop bool) func(context.Context, *Env, *Result) error {
	return func(_ context.Context, e *Env, r *Result) error {
		rp, err := loadRecording(e.Lab.Repo)
		if err != nil {
			return err
		}
		cfg := LabConfig()
		mut(&cfg)
		eng, now := engineOf(rp.recs, cfg)
		for _, id := range splitComma(ids) {
			lr := e.Prior[id]
			if lr == nil {
				return fmt.Errorf("live scenario %s missing", id)
			}
			for _, lo := range lr.Actions {
				g, err := eng.Graph(now, lo.ActionID)
				if err != nil {
					return err
				}
				o := &Outcome{Label: id + " " + lo.Label + " (replayed)", ActionID: lo.ActionID, Kind: lo.Kind, Started: lo.Started, End: lo.End,
					Truth: lo.Truth, Graph: g, unrelatedIDs: lo.unrelatedIDs}
				o.Unrelated = len(o.unrelatedIDs)
				o.Eval = Evaluate(g, o.Truth, o.unrelatedIDs)
				r.Actions = append(r.Actions, o)
				r.check(o.Label+": no false attachments", len(o.Eval.FalseAttachments) == 0, "%v", o.Eval.FalseAttachments)
				r.check(o.Label+": structural precision", o.Eval.Structural.FP == 0, "%+v", o.Eval.Structural)
				r.check(o.Label+": direct precision", o.Eval.Direct.FP == 0, "%+v", o.Eval.Direct)
				rec := o.Eval.Structural.Recall()
				live := lo.Eval.Structural.Recall()
				if rec != nil && live != nil {
					r.note("%s: structural recall %.2f (live window %.2f), ambiguities %d (live %d).", o.Label, *rec, *live, o.Eval.Ambiguities, lo.Eval.Ambiguities)
					if expectRecallDrop {
						r.check(o.Label+": shorter window lowers recall, not precision", *rec <= *live, "recall %.2f vs %.2f", *rec, *live)
					}
				}
			}
		}
		if len(r.Actions) == 0 {
			return errors.New("no actions evaluated")
		}
		return nil
	}
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// UnsupportedScenarios lists experiments that are not implemented honestly
// yet. They are reported as unsupported, never as passed.
func UnsupportedScenarios() []Scenario {
	return []Scenario{
		{ID: "U01", Title: "HorizontalPodAutoscaler changes replicas after load", Category: "unsupported",
			Description: "Requires metrics-server in the lab. EffectTrace treats the HPA controller's scale requests as actions (it is configured as an actor), but no lab experiment exercises it in v0.1."},
		{ID: "U02", Title: "GitOps-driven change", Category: "unsupported",
			Description: "No GitOps integration is implemented. A GitOps controller's requests would appear as KUBERNETES_API_CALL actions of its service account, without commit-level context."},
	}
}
