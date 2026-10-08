// Package otlp implements an OTLP/HTTP trace receiver (POST /v1/traces) that
// accepts protobuf and JSON encodings, optionally gzip-compressed.
//
// The receiver keeps only spans EffectTrace correlates: MCP tools/call spans
// and Kubernetes client spans carrying effecttrace.k8s.* attributes. Of those,
// only an allowlist of attributes is retained; tool call arguments and
// results are always dropped, even when a producer opted in to sending them.
package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/internal/store"
	"github.com/effecttrace/effecttrace/pkg/model"
	"github.com/effecttrace/effecttrace/pkg/semconv"
)

// Limits for one export request.
const (
	MaxBodyBytes       = 8 << 20
	MaxSpansPerRequest = 10_000
)

// allowedAttributes are the only span attributes retained.
var allowedAttributes = map[string]bool{
	semconv.MCPMethodName:            true,
	semconv.MCPProtocolVersion:       true,
	semconv.GenAIToolName:            true,
	semconv.GenAIOperationName:       true,
	semconv.JSONRPCRequestID:         true,
	semconv.ErrorType:                true,
	semconv.HTTPRequestMethod:        true,
	semconv.HTTPResponseStatusCode:   true,
	semconv.K8sAuditID:               true,
	semconv.K8sVerb:                  true,
	semconv.K8sAPIGroup:              true,
	semconv.K8sResource:              true,
	semconv.K8sSubresource:           true,
	semconv.K8sObjectNamespace:       true,
	semconv.K8sObjectName:            true,
	semconv.K8sObjectKind:            true,
	semconv.K8sObjectUID:             true,
	semconv.K8sObjectResourceVersion: true,
	semconv.K8sObjectGeneration:      true,
}

// Stats are cumulative receiver counters.
type Stats struct {
	Requests  atomic.Uint64
	Accepted  atomic.Uint64
	Ignored   atomic.Uint64
	Rejected  atomic.Uint64
	Throttled atomic.Uint64
}

// Receiver is an http.Handler for /v1/traces.
type Receiver struct {
	// Offer enqueues a record without blocking; it returns false when the
	// pipeline is full, which the receiver reports as HTTP 503.
	Offer  func(obs.Record) bool
	Logger *slog.Logger
	Stats  Stats
}

func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.Stats.Requests.Add(1)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mt != "application/x-protobuf" && mt != "application/json") {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var spans []*obs.Span
	var rejected int
	if mt == "application/json" {
		spans, rejected, err = DecodeJSON(body)
	} else {
		spans, rejected, err = DecodeProto(body)
	}
	if err != nil {
		rc.Stats.Rejected.Add(1)
		http.Error(w, "invalid OTLP payload", http.StatusBadRequest)
		return
	}
	rc.Stats.Rejected.Add(uint64(max(rejected, 0))) // #nosec G115 -- non-negative count
	for i, sp := range spans {
		if !rc.Offer(obs.Record{Kind: obs.KindSpan, Span: sp}) {
			rc.Stats.Throttled.Add(uint64(len(spans) - i)) // #nosec G115 -- i < len(spans)
			w.Header().Set("Retry-After", "1")
			http.Error(w, "pipeline full", http.StatusServiceUnavailable)
			return
		}
		rc.Stats.Accepted.Add(1)
	}
	writeResponse(w, mt, int64(rejected))
}

// readBody reads at most MaxBodyBytes, decompressing gzip with the same bound
// applied to the decompressed size.
func readBody(r *http.Request) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return nil, errors.New("invalid gzip body")
		}
		defer func() { _ = zr.Close() }()
		rd = zr
	default:
		return nil, errors.New("unsupported content encoding")
	}
	b, err := io.ReadAll(io.LimitReader(rd, MaxBodyBytes+1))
	if err != nil {
		return nil, errors.New("cannot read body")
	}
	if len(b) > MaxBodyBytes {
		return nil, errors.New("body too large")
	}
	return b, nil
}

func writeResponse(w http.ResponseWriter, mt string, rejected int64) {
	resp := &coltrace.ExportTraceServiceResponse{}
	if rejected > 0 {
		resp.PartialSuccess = &coltrace.ExportTracePartialSuccess{RejectedSpans: rejected, ErrorMessage: "spans failed EffectTrace validation"}
	}
	w.Header().Set("Content-Type", mt)
	if mt == "application/json" {
		out := map[string]any{}
		if rejected > 0 {
			out["partialSuccess"] = map[string]any{"rejectedSpans": strconv.FormatInt(rejected, 10), "errorMessage": resp.PartialSuccess.ErrorMessage}
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	b, _ := proto.Marshal(resp)
	_, _ = w.Write(b)
}

// DecodeProto decodes an ExportTraceServiceRequest. It returns the relevant,
// valid spans and the number of relevant spans rejected by validation.
func DecodeProto(body []byte) ([]*obs.Span, int, error) {
	var req coltrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		return nil, 0, err
	}
	var out []*obs.Span
	rejected, total := 0, 0
	for _, rs := range req.GetResourceSpans() {
		service := ""
		for _, kv := range rs.GetResource().GetAttributes() {
			if kv.GetKey() == semconv.ServiceName {
				service = anyString(kv.GetValue())
			}
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				total++
				if total > MaxSpansPerRequest {
					return nil, 0, fmt.Errorf("more than %d spans", MaxSpansPerRequest)
				}
				attrs := map[string]string{}
				for _, kv := range s.GetAttributes() {
					if allowedAttributes[kv.GetKey()] {
						attrs[kv.GetKey()] = anyString(kv.GetValue())
					}
				}
				sp := &obs.Span{
					TraceID:      hex.EncodeToString(s.GetTraceId()),
					SpanID:       hex.EncodeToString(s.GetSpanId()),
					ParentSpanID: hex.EncodeToString(s.GetParentSpanId()),
					Name:         s.GetName(),
					Kind:         obs.SpanKind(s.GetKind()),
					Service:      service,
					Start:        unixNano(s.GetStartTimeUnixNano()),
					End:          unixNano(s.GetEndTimeUnixNano()),
					Error:        s.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR,
					Attributes:   attrs,
				}
				for _, l := range s.GetLinks() {
					sp.Links = append(sp.Links, model.TraceRef{TraceID: hex.EncodeToString(l.GetTraceId()), SpanID: hex.EncodeToString(l.GetSpanId())})
				}
				keep, ok := filter(sp)
				if !keep {
					continue
				}
				if !ok {
					rejected++
					continue
				}
				out = append(out, sp)
			}
		}
	}
	return out, rejected, nil
}

// relevant reports whether EffectTrace correlates this span.
func relevant(sp *obs.Span) bool {
	return sp.Attributes[semconv.MCPMethodName] == semconv.MCPMethodToolsCall || sp.Attributes[semconv.K8sVerb] != ""
}

// filter returns keep=false for irrelevant spans, and ok=false for relevant
// spans that fail validation.
func filter(sp *obs.Span) (keep, ok bool) {
	if !relevant(sp) {
		return false, true
	}
	if len(sp.Attributes) == 0 {
		sp.Attributes = nil
	}
	if len(sp.Links) > store.MaxLinks {
		sp.Links = sp.Links[:store.MaxLinks]
	}
	err := store.Validate(obs.Record{Kind: obs.KindSpan, Span: sp})
	return true, err == nil
}

func unixNano(n uint64) time.Time {
	if n == 0 || n > 1<<62 {
		return time.Time{}
	}
	return time.Unix(0, int64(n)).UTC()
}

func anyString(v *common.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *common.AnyValue_StringValue:
		return x.StringValue
	case *common.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *common.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *common.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	default:
		return ""
	}
}

// JSON encoding (OTLP/JSON): IDs are hex strings, enums are integers and
// 64-bit integers may be strings or numbers.

type jsonRequest struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []jsonKV `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []jsonSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

type jsonKV struct {
	Key   string    `json:"key"`
	Value jsonValue `json:"value"`
}

type jsonValue struct {
	StringValue *string      `json:"stringValue"`
	IntValue    *json.Number `json:"intValue"`
	BoolValue   *bool        `json:"boolValue"`
	DoubleValue *float64     `json:"doubleValue"`
}

func (v jsonValue) String() string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return v.IntValue.String()
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
	}
	return ""
}

type jsonSpan struct {
	TraceID      string      `json:"traceId"`
	SpanID       string      `json:"spanId"`
	ParentSpanID string      `json:"parentSpanId"`
	Name         string      `json:"name"`
	Kind         int         `json:"kind"`
	Start        json.Number `json:"startTimeUnixNano"`
	End          json.Number `json:"endTimeUnixNano"`
	Attributes   []jsonKV    `json:"attributes"`
	Status       struct {
		Code int `json:"code"`
	} `json:"status"`
	Links []struct {
		TraceID string `json:"traceId"`
		SpanID  string `json:"spanId"`
	} `json:"links"`
}

// DecodeJSON decodes an OTLP/JSON ExportTraceServiceRequest.
func DecodeJSON(body []byte) ([]*obs.Span, int, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req jsonRequest
	if err := dec.Decode(&req); err != nil {
		return nil, 0, err
	}
	var out []*obs.Span
	rejected, total := 0, 0
	for _, rs := range req.ResourceSpans {
		service := ""
		for _, kv := range rs.Resource.Attributes {
			if kv.Key == semconv.ServiceName {
				service = kv.Value.String()
			}
		}
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				total++
				if total > MaxSpansPerRequest {
					return nil, 0, fmt.Errorf("more than %d spans", MaxSpansPerRequest)
				}
				attrs := map[string]string{}
				for _, kv := range s.Attributes {
					if allowedAttributes[kv.Key] {
						attrs[kv.Key] = kv.Value.String()
					}
				}
				sp := &obs.Span{
					TraceID:      strings.ToLower(s.TraceID),
					SpanID:       strings.ToLower(s.SpanID),
					ParentSpanID: strings.ToLower(s.ParentSpanID),
					Name:         s.Name,
					Kind:         obs.SpanKind(s.Kind),
					Service:      service,
					Start:        jsonTime(s.Start),
					End:          jsonTime(s.End),
					Error:        s.Status.Code == int(tracepb.Status_STATUS_CODE_ERROR),
					Attributes:   attrs,
				}
				for _, l := range s.Links {
					sp.Links = append(sp.Links, model.TraceRef{TraceID: strings.ToLower(l.TraceID), SpanID: strings.ToLower(l.SpanID)})
				}
				keep, ok := filter(sp)
				if !keep {
					continue
				}
				if !ok {
					rejected++
					continue
				}
				out = append(out, sp)
			}
		}
	}
	return out, rejected, nil
}

func jsonTime(n json.Number) time.Time {
	v, err := strconv.ParseUint(n.String(), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return unixNano(v)
}

// ListenAndServe is a helper for tests and the collector.
func ListenAndServe(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second}
	go func() { // #nosec G118 -- server shutdown goroutine, not request-scoped
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // #nosec G118 -- shutdown outlives the cancelled context
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
