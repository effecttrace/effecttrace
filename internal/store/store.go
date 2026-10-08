// Package store holds EffectTrace's bounded, in-memory observation state.
//
// Every write is an idempotent, keyed upsert: replayed or duplicated records
// are counted and ignored. All collections are bounded by count and by age so
// that a flood of telemetry cannot exhaust memory.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/pkg/model"
)

// ErrInvalid is returned for records that fail validation.
var ErrInvalid = errors.New("invalid record")

// Config bounds the store.
type Config struct {
	Retention             time.Duration
	MaxRequests           int
	MaxSpans              int
	MaxObjects            int
	MaxObservationsPerObj int
	MaxEvents             int
	MaxMetricResults      int
}

// DefaultConfig returns conservative bounds suitable for a single cluster.
func DefaultConfig() Config {
	return Config{
		Retention:   2 * time.Hour,
		MaxRequests: 50_000,
		MaxSpans:    100_000,
		MaxObjects:  50_000,
		// A hard guard only: normal eviction is by age (Prune), which keeps
		// graphs a deterministic function of the retained observations.
		MaxObservationsPerObj: 2048,
		MaxEvents:             50_000,
		MaxMetricResults:      20_000,
	}
}

// ObjectHistory is the observed lifetime of one object UID.
type ObjectHistory struct {
	Ref          model.ObjectRef
	Observations []obs.ObjectObservation
	// Dropped counts observations discarded because of the per-object cap.
	Dropped int
}

// First returns the earliest retained observation.
func (h *ObjectHistory) First() obs.ObjectObservation { return h.Observations[0] }

// Last returns the latest retained observation.
func (h *ObjectHistory) Last() obs.ObjectObservation { return h.Observations[len(h.Observations)-1] }

// CreatedAt returns when the object came into existence as best known: the
// collector observation time for objects created while watched, otherwise the
// server creationTimestamp.
func (h *ObjectHistory) CreatedAt() time.Time {
	f := h.First()
	if f.Initial || f.Type != obs.WatchAdded {
		return f.CreatedAt
	}
	return f.At
}

// DeletedAt returns when deletion was first observed (deletionTimestamp set
// or DELETED notification), or zero.
func (h *ObjectHistory) DeletedAt() time.Time {
	for _, o := range h.Observations {
		if o.Type == obs.WatchDeleted || !o.DeletingAt.IsZero() {
			if o.Initial {
				return o.DeletingAt
			}
			return o.At
		}
	}
	return time.Time{}
}

// ControllerOwner returns the controller owner of the object, if any.
func (h *ObjectHistory) ControllerOwner() (obs.OwnerRef, bool) {
	for i := len(h.Observations) - 1; i >= 0; i-- {
		for _, o := range h.Observations[i].Owners {
			if o.Controller {
				return o, true
			}
		}
	}
	return obs.OwnerRef{}, false
}

// Stats counts ingest outcomes by record kind.
type Stats struct {
	Applied   map[obs.Kind]uint64
	Duplicate map[obs.Kind]uint64
	Rejected  map[obs.Kind]uint64
	Evicted   map[obs.Kind]uint64
	// AuditIDReuse counts Audit-IDs that became untrusted because more
	// requests than MaxRequestsPerAuditID carried them.
	AuditIDReuse uint64
}

type spanKey struct{ trace, span string }

type nameKey struct{ kind, namespace, name string }

// Store is safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	cfg     Config
	version uint64

	requests   map[string]*obs.AuditRequest // keyed by RequestKey
	reqOrder   []string
	byAudit    map[string][]string // auditID -> request keys (at most MaxRequestsPerAuditID)
	auditCount map[string]int      // auditID -> number of stored requests carrying it
	spans      map[spanKey]*obs.Span
	spanOrder  []spanKey
	byTrace    map[string][]spanKey
	objects    map[string]*ObjectHistory
	objOrder   []string
	byName     map[nameKey][]string
	children   map[string][]string
	events     map[string]*obs.EventObservation
	occ        map[string][]Occurrence
	evOrder    []string
	byRegard   map[string][]string
	metrics    map[string][]obs.MetricResult
	metricN    int
	sources    map[string][]obs.SourceStatus
	stats      Stats
	newestSeen time.Time
}

// New returns an empty store.
func New(cfg Config) *Store {
	return &Store{
		cfg:        cfg,
		requests:   map[string]*obs.AuditRequest{},
		byAudit:    map[string][]string{},
		auditCount: map[string]int{},
		spans:      map[spanKey]*obs.Span{},
		byTrace:    map[string][]spanKey{},
		objects:    map[string]*ObjectHistory{},
		byName:     map[nameKey][]string{},
		children:   map[string][]string{},
		events:     map[string]*obs.EventObservation{},
		occ:        map[string][]Occurrence{},
		byRegard:   map[string][]string{},
		metrics:    map[string][]obs.MetricResult{},
		sources:    map[string][]obs.SourceStatus{},
		stats: Stats{
			Applied:   map[obs.Kind]uint64{},
			Duplicate: map[obs.Kind]uint64{},
			Rejected:  map[obs.Kind]uint64{},
			Evicted:   map[obs.Kind]uint64{},
		},
	}
}

// Apply ingests one record. It returns false without error for duplicates.
func (s *Store) Apply(r obs.Record) (bool, error) {
	if err := Validate(r); err != nil {
		s.mu.Lock()
		s.stats.Rejected[r.Kind]++
		s.mu.Unlock()
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var applied bool
	switch r.Kind {
	case obs.KindAudit:
		applied, _ = s.applyAudit(r.Audit) // never fails: records are always kept
	case obs.KindSpan:
		applied = s.applySpan(r.Span)
	case obs.KindObject:
		applied = s.applyObject(r.Object)
	case obs.KindEvent:
		applied = s.applyEvent(r.Event)
	case obs.KindMetric:
		applied = s.applyMetric(r.Metric)
	case obs.KindSource:
		applied = s.applySource(r.Source)
	}
	if applied {
		s.version++
		s.stats.Applied[r.Kind]++
	} else {
		s.stats.Duplicate[r.Kind]++
	}
	return applied, nil
}

func (s *Store) touch(t time.Time) {
	if t.After(s.newestSeen) {
		s.newestSeen = t
	}
}

// RequestKey identifies one audit event. kube-apiserver accepts
// client-supplied Audit-IDs, so an ID alone is not unique or trustworthy: a
// client could reuse another request's ID. The key therefore also includes
// the server-recorded request facts, so a request that merely reuses an ID
// can never replace or merge with another request.
func RequestKey(a *obs.AuditRequest) string {
	return a.AuditID + "|" + a.Verb + "|" + a.APIGroup + "|" + a.Resource + "|" + a.Subresource + "|" +
		a.Namespace + "|" + a.Name + "|" + a.ReceivedAt.UTC().Format(time.RFC3339Nano)
}

// MaxRequestsPerAuditID bounds the per-Audit-ID index. Every audit record is
// always stored (dropping one would let a client hide a real mutation by
// pre-filling its ID); an ID carried by more requests than this is treated
// as untrusted and confirms nothing.
const MaxRequestsPerAuditID = 8

func (s *Store) applyAudit(a *obs.AuditRequest) (bool, error) {
	key := RequestKey(a)
	if prev, ok := s.requests[key]; ok {
		// The same audit event read again (for example after a collector
		// restart re-reads the log). Only the pseudonym can differ, if the
		// pseudonymization key changed; keep the lexically smaller one so the
		// result does not depend on arrival order.
		if a.User < prev.User {
			c := *a
			s.requests[key] = &c
			return true, nil
		}
		return false, nil
	}
	c := *a
	s.requests[key] = &c
	s.reqOrder = append(s.reqOrder, key)
	s.auditCount[a.AuditID]++
	if s.auditCount[a.AuditID] == MaxRequestsPerAuditID+1 {
		s.stats.AuditIDReuse++
	}
	if keys := s.byAudit[a.AuditID]; len(keys) < MaxRequestsPerAuditID {
		i, _ := slices.BinarySearch(keys, key)
		s.byAudit[a.AuditID] = slices.Insert(keys, i, key)
	}
	s.touch(a.ReceivedAt)
	for len(s.reqOrder) > s.cfg.MaxRequests {
		s.evictRequest(s.reqOrder[0])
		s.reqOrder = s.reqOrder[1:]
	}
	return true, nil
}

func (s *Store) evictRequest(key string) {
	r, ok := s.requests[key]
	if !ok {
		return
	}
	delete(s.requests, key)
	if s.auditCount[r.AuditID]--; s.auditCount[r.AuditID] <= 0 {
		delete(s.auditCount, r.AuditID)
	}
	keys := slices.DeleteFunc(s.byAudit[r.AuditID], func(k string) bool { return k == key })
	if len(keys) == 0 {
		delete(s.byAudit, r.AuditID)
	} else {
		s.byAudit[r.AuditID] = keys
	}
	s.stats.Evicted[obs.KindAudit]++
}

func (s *Store) applySpan(sp *obs.Span) bool {
	k := spanKey{sp.TraceID, sp.SpanID}
	if _, ok := s.spans[k]; ok {
		return false
	}
	c := *sp
	s.spans[k] = &c
	s.spanOrder = append(s.spanOrder, k)
	s.byTrace[sp.TraceID] = append(s.byTrace[sp.TraceID], k)
	s.touch(sp.Start)
	for len(s.spanOrder) > s.cfg.MaxSpans {
		s.evictSpan(s.spanOrder[0])
		s.spanOrder = s.spanOrder[1:]
	}
	return true
}

func (s *Store) evictSpan(k spanKey) {
	delete(s.spans, k)
	keys := s.byTrace[k.trace]
	keys = slices.DeleteFunc(keys, func(x spanKey) bool { return x == k })
	if len(keys) == 0 {
		delete(s.byTrace, k.trace)
	} else {
		s.byTrace[k.trace] = keys
	}
	s.stats.Evicted[obs.KindSpan]++
}

func (s *Store) applyObject(o *obs.ObjectObservation) bool {
	uid := o.Ref.UID
	h, ok := s.objects[uid]
	if !ok {
		h = &ObjectHistory{Ref: o.Ref}
		s.objects[uid] = h
		s.objOrder = append(s.objOrder, uid)
		nk := nameKey{o.Ref.Kind, o.Ref.Namespace, o.Ref.Name}
		s.byName[nk] = append(s.byName[nk], uid)
		for len(s.objOrder) > s.cfg.MaxObjects {
			s.evictObject(s.objOrder[0])
			s.objOrder = s.objOrder[1:]
		}
	}
	for i, prev := range h.Observations {
		if prev.ResourceVersion == o.ResourceVersion && prev.Type == o.Type {
			// The same notification seen again (for example a relist after a
			// collector restart). Keep the earliest sighting so the result
			// does not depend on arrival order.
			if !o.At.Before(prev.At) {
				return false
			}
			h.Observations = slices.Delete(h.Observations, i, i+1)
			break
		}
	}
	// Insert in observation-time order; replays may arrive out of order.
	i, _ := slices.BinarySearchFunc(h.Observations, o.At, func(x obs.ObjectObservation, t time.Time) int {
		return x.At.Compare(t)
	})
	h.Observations = slices.Insert(h.Observations, i, *o)
	if over := len(h.Observations) - s.cfg.MaxObservationsPerObj; over > 0 {
		// Keep the first observation (creation) and the most recent ones.
		h.Observations = append(h.Observations[:1], h.Observations[1+over:]...)
		h.Dropped += over
	}
	for _, ow := range o.Owners {
		if ow.Controller && !slices.Contains(s.children[ow.UID], uid) {
			s.children[ow.UID] = append(s.children[ow.UID], uid)
		}
	}
	s.touch(o.At)
	return true
}

func (s *Store) evictObject(uid string) {
	h, ok := s.objects[uid]
	if !ok {
		return
	}
	delete(s.objects, uid)
	nk := nameKey{h.Ref.Kind, h.Ref.Namespace, h.Ref.Name}
	s.byName[nk] = slices.DeleteFunc(s.byName[nk], func(x string) bool { return x == uid })
	if len(s.byName[nk]) == 0 {
		delete(s.byName, nk)
	}
	delete(s.children, uid)
	s.stats.Evicted[obs.KindObject]++
}

// MaxSourceStatuses bounds the status history kept per source.
const MaxSourceStatuses = 256

// applySource keeps a bounded history of source reports. Reports are
// deduplicated by time and instance start, so replays are idempotent.
func (s *Store) applySource(st *obs.SourceStatus) bool {
	list := s.sources[st.Source]
	for _, p := range list {
		if p.At.Equal(st.At) && p.StartedAt.Equal(st.StartedAt) && p.Healthy == st.Healthy {
			return false
		}
	}
	list = append(list, *st)
	slices.SortFunc(list, func(a, b obs.SourceStatus) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return a.StartedAt.Compare(b.StartedAt)
	})
	if len(list) > MaxSourceStatuses {
		// Keep the first report of every instance and the newest reports.
		seen := map[time.Time]bool{}
		var keep []obs.SourceStatus
		for _, p := range list {
			if !seen[p.StartedAt] {
				seen[p.StartedAt] = true
				keep = append(keep, p)
			}
		}
		for _, p := range list[len(list)-MaxSourceStatuses/2:] {
			if !slices.ContainsFunc(keep, func(k obs.SourceStatus) bool { return k.At.Equal(p.At) && k.StartedAt.Equal(p.StartedAt) }) {
				keep = append(keep, p)
			}
		}
		slices.SortFunc(keep, func(a, b obs.SourceStatus) int { return a.At.Compare(b.At) })
		list = keep
	}
	s.sources[st.Source] = list
	return true
}

// Occurrence is one observed occurrence of an Event series.
type Occurrence struct {
	At      time.Time
	Initial bool
}

// MaxOccurrences bounds the occurrences kept per Event series (the newest).
const MaxOccurrences = 64

// addOccurrence records an occurrence. Kubernetes aggregates repeated Events
// into one object whose count grows, so a series can span several unrelated
// episodes; graphs match occurrences, not just the first time, against
// windows. The kept set is the newest MaxOccurrences, independent of order.
func (s *Store) addOccurrence(uid string, o Occurrence) bool {
	list := s.occ[uid]
	for _, p := range list {
		if p.At.Equal(o.At) && p.Initial == o.Initial {
			return false
		}
	}
	list = append(list, o)
	slices.SortFunc(list, func(a, b Occurrence) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		if a.Initial == b.Initial {
			return 0
		}
		if a.Initial {
			return 1
		}
		return -1
	})
	if len(list) > MaxOccurrences {
		list = list[len(list)-MaxOccurrences:]
	}
	s.occ[uid] = list
	return true
}

func (s *Store) applyEvent(e *obs.EventObservation) bool {
	added := s.addOccurrence(e.UID, Occurrence{At: e.At, Initial: e.Initial})
	if prev, ok := s.events[e.UID]; ok {
		// Event series update. Merge deterministically regardless of the
		// order updates arrive in: earliest time, highest count.
		changed := false
		switch {
		case prev.Initial && !e.Initial:
			// A live observation time is more precise than the server
			// timestamp of a relisted Event.
			prev.At, prev.Initial = e.At, false
			changed = true
		case prev.Initial == e.Initial && e.At.Before(prev.At):
			prev.At = e.At
			changed = true
		}
		// The note of an aggregated Event series changes as it grows; keep
		// the note of the highest count (lexically smallest on ties).
		if e.Count > prev.Count || (e.Count == prev.Count && e.Note < prev.Note) {
			prev.Count = e.Count
			prev.Note = e.Note
			changed = true
		}
		return changed || added
	}
	c := *e
	s.events[e.UID] = &c
	s.evOrder = append(s.evOrder, e.UID)
	s.byRegard[e.Regarding.UID] = append(s.byRegard[e.Regarding.UID], e.UID)
	s.touch(e.At)
	for len(s.evOrder) > s.cfg.MaxEvents {
		old := s.evOrder[0]
		s.evOrder = s.evOrder[1:]
		if ev, ok := s.events[old]; ok {
			r := ev.Regarding.UID
			s.byRegard[r] = slices.DeleteFunc(s.byRegard[r], func(x string) bool { return x == old })
			if len(s.byRegard[r]) == 0 {
				delete(s.byRegard, r)
			}
			delete(s.events, old)
			delete(s.occ, old)
			s.stats.Evicted[obs.KindEvent]++
		}
	}
	return true
}

// betterMetric orders duplicate evaluations of one signal deterministically
// (for example re-evaluations after a collector restart): the earliest
// evaluation, then without evaluation times the most samples, the latest
// window end and the lowest observed value.
func betterMetric(a, b obs.MetricResult) bool {
	// The first evaluation is the one the live graph reported; later ones
	// come from a restarted collector re-evaluating old actions.
	if !a.EvaluatedAt.Equal(b.EvaluatedAt) && !a.EvaluatedAt.IsZero() && !b.EvaluatedAt.IsZero() {
		return a.EvaluatedAt.Before(b.EvaluatedAt)
	}
	if (a.Error == "") != (b.Error == "") {
		return a.Error == ""
	}
	if a.Samples != b.Samples {
		return a.Samples > b.Samples
	}
	if !a.WindowEnd.Equal(b.WindowEnd) {
		return a.WindowEnd.After(b.WindowEnd)
	}
	if a.Observed != b.Observed {
		return a.Observed < b.Observed
	}
	return a.Baseline < b.Baseline
}

func (s *Store) applyMetric(m *obs.MetricResult) bool {
	list := s.metrics[m.ActionID]
	for i, p := range list {
		if p.Signal == m.Signal && p.Namespace == m.Namespace && p.Workload == m.Workload {
			if betterMetric(*m, p) {
				list[i] = *m
				return true
			}
			return false
		}
	}
	if s.metricN >= s.cfg.MaxMetricResults {
		return false
	}
	s.metrics[m.ActionID] = append(list, *m)
	s.metricN++
	return true
}

// Prune evicts records older than the retention period relative to now.
func (s *Store) Prune(now time.Time) {
	cut := now.Add(-s.cfg.Retention)
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for len(s.reqOrder) > 0 {
		r, ok := s.requests[s.reqOrder[0]]
		if ok && !r.ReceivedAt.Before(cut) {
			break
		}
		s.evictRequest(s.reqOrder[0])
		s.reqOrder = s.reqOrder[1:]
		changed = true
	}
	for len(s.spanOrder) > 0 {
		sp, ok := s.spans[s.spanOrder[0]]
		if ok && !sp.Start.Before(cut) {
			break
		}
		s.evictSpan(s.spanOrder[0])
		s.spanOrder = s.spanOrder[1:]
		changed = true
	}
	for len(s.objOrder) > 0 {
		h, ok := s.objects[s.objOrder[0]]
		if ok {
			d := h.DeletedAt()
			if d.IsZero() || !h.Last().At.Before(cut) {
				break
			}
		}
		s.evictObject(s.objOrder[0])
		s.objOrder = s.objOrder[1:]
		changed = true
	}
	// Drop expired observations of long-lived objects, keeping the first
	// (creation) and the latest observation.
	for _, h := range s.objects {
		n := len(h.Observations)
		if n <= 2 || !h.Observations[1].At.Before(cut) {
			continue
		}
		kept := []obs.ObjectObservation{h.Observations[0]}
		for i, o := range h.Observations[1:] {
			if !o.At.Before(cut) || i == n-2 {
				kept = append(kept, o)
			}
		}
		h.Dropped += n - len(kept)
		h.Observations = kept
		changed = true
	}
	if changed {
		s.version++
	}
}

// Version increases on every applied change; it lets readers cache results.
func (s *Store) Version() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// StatsSnapshot returns a copy of ingest counters.
func (s *Store) StatsSnapshot() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := func(m map[obs.Kind]uint64) map[obs.Kind]uint64 {
		out := make(map[obs.Kind]uint64, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return Stats{cp(s.stats.Applied), cp(s.stats.Duplicate), cp(s.stats.Rejected), cp(s.stats.Evicted), s.stats.AuditIDReuse}
}

// Read runs fn with a consistent read-only view of the store.
func (s *Store) Read(fn func(v View)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(View{s})
}

// View is a read-only view valid only inside Store.Read.
type View struct{ s *Store }

// Version returns the store version the view reflects.
func (v View) Version() uint64 { return v.s.version }

// Newest returns the latest timestamp seen in any record.
func (v View) Newest() time.Time { return v.s.newestSeen }

// Requests returns all audit requests ordered by receive time then ID.
func (v View) Requests() []*obs.AuditRequest {
	out := make([]*obs.AuditRequest, 0, len(v.s.requests))
	for _, r := range v.s.requests {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *obs.AuditRequest) int {
		if c := a.ReceivedAt.Compare(b.ReceivedAt); c != 0 {
			return c
		}
		return strings.Compare(a.AuditID, b.AuditID)
	})
	return out
}

// Request returns an audit request by ID.
// Request returns the audit request with an Audit-ID, if exactly one request
// carries it. Reused IDs return false.
func (v View) Request(auditID string) (*obs.AuditRequest, bool) {
	keys := v.trustedKeys(auditID)
	if len(keys) != 1 {
		return nil, false
	}
	return v.s.requests[keys[0]], true
}

// trustedKeys returns the indexed requests of an Audit-ID, or nil when the
// ID is carried by more requests than the index holds (untrusted).
func (v View) trustedKeys(auditID string) []string {
	keys := v.s.byAudit[auditID]
	if v.s.auditCount[auditID] != len(keys) {
		return nil
	}
	return keys
}

// AuditIDUntrusted reports whether an Audit-ID is carried by more requests
// than the index holds, so it confirms nothing.
func (v View) AuditIDUntrusted(auditID string) bool {
	return v.s.auditCount[auditID] > 0 && v.trustedKeys(auditID) == nil
}

// RequestsByAuditID returns every request carrying an Audit-ID (more than
// one only if a client reused an ID), in key order.
func (v View) RequestsByAuditID(auditID string) []*obs.AuditRequest {
	var out []*obs.AuditRequest
	for _, k := range v.trustedKeys(auditID) {
		out = append(out, v.s.requests[k])
	}
	return out
}

// RequestID returns a stable, unique identifier for an audit request: the
// Audit-ID, suffixed with a digest of the request facts when the ID is
// shared by more than one request.
func (v View) RequestID(r *obs.AuditRequest) string {
	if v.s.auditCount[r.AuditID] <= 1 {
		return r.AuditID
	}
	h := sha256.Sum256([]byte(RequestKey(r)))
	return r.AuditID + "-" + hex.EncodeToString(h[:4])
}

// Spans returns all spans ordered by start time, trace and span ID.
func (v View) Spans() []*obs.Span {
	out := make([]*obs.Span, 0, len(v.s.spans))
	for _, sp := range v.s.spans {
		out = append(out, sp)
	}
	sortSpans(out)
	return out
}

// TraceSpans returns the spans of one trace in a stable order.
func (v View) TraceSpans(traceID string) []*obs.Span {
	keys := v.s.byTrace[traceID]
	out := make([]*obs.Span, 0, len(keys))
	for _, k := range keys {
		if sp, ok := v.s.spans[k]; ok {
			out = append(out, sp)
		}
	}
	sortSpans(out)
	return out
}

func sortSpans(out []*obs.Span) {
	slices.SortFunc(out, func(a, b *obs.Span) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		if c := strings.Compare(a.TraceID, b.TraceID); c != 0 {
			return c
		}
		return strings.Compare(a.SpanID, b.SpanID)
	})
}

// Object returns the history of an object UID.
func (v View) Object(uid string) (*ObjectHistory, bool) {
	h, ok := v.s.objects[uid]
	return h, ok
}

// ObjectsByName returns the UIDs that have carried a kind/namespace/name.
func (v View) ObjectsByName(kind, namespace, name string) []string {
	return slices.Clone(v.s.byName[nameKey{kind, namespace, name}])
}

// Children returns the UIDs of objects whose controller owner is uid, sorted.
func (v View) Children(uid string) []string {
	out := slices.Clone(v.s.children[uid])
	slices.Sort(out)
	return out
}

// ObjectsInNamespace returns the histories of objects in a namespace, sorted
// by kind and name.
func (v View) ObjectsInNamespace(ns string) []*ObjectHistory {
	var out []*ObjectHistory
	for _, h := range v.s.objects {
		if h.Ref.Namespace == ns {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b *ObjectHistory) int {
		return compareRefs(a.Ref, b.Ref)
	})
	return out
}

func compareRefs(a, b model.ObjectRef) int {
	for _, c := range [][2]string{{a.Kind, b.Kind}, {a.Name, b.Name}, {a.UID, b.UID}} {
		if r := strings.Compare(c[0], c[1]); r != 0 {
			return r
		}
	}
	return 0
}

// EventsRegarding returns events that reference an object UID, by time.
func (v View) EventsRegarding(uid string) []*obs.EventObservation {
	ids := v.s.byRegard[uid]
	out := make([]*obs.EventObservation, 0, len(ids))
	for _, id := range ids {
		if e, ok := v.s.events[id]; ok {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b *obs.EventObservation) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(a.UID, b.UID)
	})
	return out
}

// EventOccurrences returns the occurrences of an Event series, oldest first.
func (v View) EventOccurrences(uid string) []Occurrence {
	return slices.Clone(v.s.occ[uid])
}

// Metrics returns metric results recorded for an action.
func (v View) Metrics(actionID string) []obs.MetricResult {
	out := slices.Clone(v.s.metrics[actionID])
	slices.SortFunc(out, func(a, b obs.MetricResult) int {
		return strings.Compare(a.Workload+"\x00"+a.Signal, b.Workload+"\x00"+b.Signal)
	})
	return out
}

// Source returns the latest reported status of a source.
func (v View) Source(name string) (obs.SourceStatus, bool) {
	list := v.s.sources[name]
	if len(list) == 0 {
		return obs.SourceStatus{}, false
	}
	return list[len(list)-1], true
}

// SourceInstances returns the distinct start times of a source's instances
// (for example collector restarts), oldest first.
func (v View) SourceInstances(name string) []time.Time {
	var out []time.Time
	for _, st := range v.s.sources[name] {
		if !st.StartedAt.IsZero() && !slices.ContainsFunc(out, func(t time.Time) bool { return t.Equal(st.StartedAt) }) {
			out = append(out, st.StartedAt)
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}

// Counts returns current collection sizes.
func (v View) Counts() map[string]int {
	return map[string]int{
		"requests": len(v.s.requests),
		"spans":    len(v.s.spans),
		"objects":  len(v.s.objects),
		"events":   len(v.s.events),
		"metrics":  v.s.metricN,
	}
}

// String implements fmt.Stringer for debugging.
func (s *Store) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("store{requests=%d spans=%d objects=%d events=%d}", len(s.requests), len(s.spans), len(s.objects), len(s.events))
}
