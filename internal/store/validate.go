package store

import (
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/effecttrace/effecttrace/internal/obs"
)

// Input bounds. Values beyond these are rejected rather than truncated so
// that identifiers are never silently altered.
const (
	MaxIDLen         = 128
	MaxNameLen       = 253
	MaxTextLen       = 1024
	MaxAttributes    = 32
	MaxAttrKeyLen    = 128
	MaxAttrValueLen  = 512
	MaxLinks         = 16
	MaxOwners        = 8
	minPlausibleYear = 2000
	maxPlausibleYear = 2200
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// cleanString reports whether s is valid UTF-8 within max bytes and free of
// control characters. Control characters are rejected because EffectTrace
// prints identifiers to terminals, where escape sequences could spoof output.
func cleanString(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	zero := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
		if c != '0' {
			zero = false
		}
	}
	return !zero
}

// ValidTraceID reports whether s is a non-zero lowercase 16-byte hex ID.
func ValidTraceID(s string) bool { return isHex(s, 32) }

// ValidSpanID reports whether s is a non-zero lowercase 8-byte hex ID.
func ValidSpanID(s string) bool { return isHex(s, 16) }

func plausible(t time.Time) bool {
	y := t.Year()
	return !t.IsZero() && y >= minPlausibleYear && y <= maxPlausibleYear
}

func checkStrings(max int, fields map[string]string) error {
	for name, v := range fields {
		if !cleanString(v, max) {
			return invalid("field %s is too long or contains control characters", name)
		}
	}
	return nil
}

// Validate checks a record against EffectTrace's input bounds.
func Validate(r obs.Record) error {
	set := 0
	for _, p := range []bool{r.Audit != nil, r.Span != nil, r.Object != nil, r.Event != nil, r.Metric != nil, r.Source != nil} {
		if p {
			set++
		}
	}
	if set != 1 {
		return invalid("record must carry exactly one payload")
	}
	switch r.Kind {
	case obs.KindAudit:
		if r.Audit == nil {
			return invalid("kind audit without payload")
		}
		return validateAudit(r.Audit)
	case obs.KindSpan:
		if r.Span == nil {
			return invalid("kind span without payload")
		}
		return validateSpan(r.Span)
	case obs.KindObject:
		if r.Object == nil {
			return invalid("kind object without payload")
		}
		return validateObject(r.Object)
	case obs.KindEvent:
		if r.Event == nil {
			return invalid("kind event without payload")
		}
		return validateEvent(r.Event)
	case obs.KindMetric:
		if r.Metric == nil {
			return invalid("kind metric without payload")
		}
		return validateMetric(r.Metric)
	case obs.KindSource:
		if r.Source == nil || r.Source.Source == "" {
			return invalid("kind source without payload")
		}
		return checkStrings(MaxTextLen, map[string]string{"source": r.Source.Source, "detail": r.Source.Detail})
	default:
		return invalid("unknown kind %q", r.Kind)
	}
}

func validateAudit(a *obs.AuditRequest) error {
	if a.AuditID == "" || a.Verb == "" || a.Resource == "" {
		return invalid("audit request requires auditId, verb and resource")
	}
	if !plausible(a.ReceivedAt) {
		return invalid("audit request has implausible receive time")
	}
	if err := checkStrings(MaxIDLen, map[string]string{"auditId": a.AuditID, "verb": a.Verb, "objectUid": a.ObjectUID}); err != nil {
		return err
	}
	return checkStrings(MaxNameLen, map[string]string{
		"apiGroup": a.APIGroup, "apiVersion": a.APIVersion, "resource": a.Resource,
		"subresource": a.Subresource, "namespace": a.Namespace, "name": a.Name,
		"user": a.User, "userAgent": a.UserAgent,
	})
}

func validateSpan(sp *obs.Span) error {
	if !ValidTraceID(sp.TraceID) || !ValidSpanID(sp.SpanID) {
		return invalid("span requires valid trace and span IDs")
	}
	if sp.ParentSpanID != "" && !ValidSpanID(sp.ParentSpanID) {
		return invalid("span has invalid parent span ID")
	}
	if !plausible(sp.Start) || (!sp.End.IsZero() && sp.End.Before(sp.Start)) {
		return invalid("span has implausible timing")
	}
	if err := checkStrings(MaxNameLen, map[string]string{"name": sp.Name, "service": sp.Service}); err != nil {
		return err
	}
	if len(sp.Links) > MaxLinks {
		return invalid("span has too many links")
	}
	for _, l := range sp.Links {
		if !ValidTraceID(l.TraceID) || !ValidSpanID(l.SpanID) {
			return invalid("span link has invalid IDs")
		}
	}
	if len(sp.Attributes) > MaxAttributes {
		return invalid("span has too many attributes")
	}
	for k, v := range sp.Attributes {
		if !cleanString(k, MaxAttrKeyLen) || !cleanString(v, MaxAttrValueLen) {
			return invalid("span attribute %q is too long or contains control characters", k)
		}
	}
	return nil
}

func validateObject(o *obs.ObjectObservation) error {
	if o.Ref.UID == "" || o.Ref.Kind == "" || o.Ref.Name == "" || o.ResourceVersion == "" {
		return invalid("object observation requires uid, kind, name and resourceVersion")
	}
	switch o.Type {
	case obs.WatchAdded, obs.WatchModified, obs.WatchDeleted:
	default:
		return invalid("object observation has unknown type %q", o.Type)
	}
	if !plausible(o.At) {
		return invalid("object observation has implausible time")
	}
	if len(o.Owners) > MaxOwners {
		return invalid("object has too many owners")
	}
	f := map[string]string{
		"uid": o.Ref.UID, "resourceVersion": o.ResourceVersion, "revision": o.Revision, "templateHash": o.TemplateHash,
	}
	if err := checkStrings(MaxIDLen, f); err != nil {
		return err
	}
	if err := checkStrings(MaxNameLen, map[string]string{
		"kind": o.Ref.Kind, "apiVersion": o.Ref.APIVersion, "namespace": o.Ref.Namespace, "name": o.Ref.Name, "workload": o.Workload,
	}); err != nil {
		return err
	}
	for _, ow := range o.Owners {
		if ow.UID == "" || !cleanString(ow.UID, MaxIDLen) || !cleanString(ow.Kind, MaxNameLen) || !cleanString(ow.Name, MaxNameLen) {
			return invalid("object has invalid owner reference")
		}
		if ow.UID == o.Ref.UID {
			return invalid("object cannot own itself")
		}
	}
	return nil
}

func validateEvent(e *obs.EventObservation) error {
	if e.UID == "" || e.Regarding.UID == "" || e.Reason == "" {
		return invalid("event requires uid, regarding uid and reason")
	}
	if !plausible(e.At) {
		return invalid("event has implausible time")
	}
	if err := checkStrings(MaxIDLen, map[string]string{"uid": e.UID, "regardingUid": e.Regarding.UID}); err != nil {
		return err
	}
	if err := checkStrings(MaxNameLen, map[string]string{
		"reason": e.Reason, "type": e.Type, "controller": e.Controller,
		"kind": e.Regarding.Kind, "namespace": e.Regarding.Namespace, "name": e.Regarding.Name,
	}); err != nil {
		return err
	}
	return checkStrings(MaxTextLen, map[string]string{"note": e.Note})
}

func validateMetric(m *obs.MetricResult) error {
	if m.ActionID == "" || m.Signal == "" || m.Workload == "" {
		return invalid("metric result requires actionId, signal and workload")
	}
	return checkStrings(MaxNameLen, map[string]string{
		"actionId": m.ActionID, "signal": m.Signal, "unit": m.Unit, "namespace": m.Namespace,
		"workload": m.Workload, "workloadUid": m.WorkloadUID, "direction": m.Direction, "error": m.Error,
	})
}
