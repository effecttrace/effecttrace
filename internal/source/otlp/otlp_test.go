package otlp

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/effecttrace/effecttrace/internal/obs"
	"github.com/effecttrace/effecttrace/pkg/semconv"
)

func kv(k, v string) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: v}}}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func request() *coltrace.ExportTraceServiceRequest {
	trace := mustHex("4bf92f3577b34da6a3ce929d0e0e4736")
	return &coltrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resource.Resource{Attributes: []*common.KeyValue{kv("service.name", "demo-tools")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
			{
				TraceId: trace, SpanId: mustHex("00f067aa0ba902b7"), Name: "tools/call restart_workload",
				Kind: tracepb.Span_SPAN_KIND_SERVER, StartTimeUnixNano: 1_790_000_000_000_000_000, EndTimeUnixNano: 1_790_000_000_100_000_000,
				Attributes: []*common.KeyValue{
					kv(semconv.MCPMethodName, "tools/call"), kv(semconv.GenAIToolName, "restart_workload"),
					kv("gen_ai.tool.call.arguments", `{"secret":"hunter2"}`), kv("gen_ai.tool.call.result", "token=abc"),
				},
			},
			{ // irrelevant span: ignored
				TraceId: trace, SpanId: mustHex("1111111111111111"), Name: "GET /healthz",
				StartTimeUnixNano: 1_790_000_000_000_000_000,
			},
			{ // forged: zero trace id
				TraceId: make([]byte, 16), SpanId: mustHex("2222222222222222"), Name: "tools/call x",
				StartTimeUnixNano: 1_790_000_000_000_000_000, Attributes: []*common.KeyValue{kv(semconv.MCPMethodName, "tools/call")},
			},
			{ // control characters in name: rejected
				TraceId: trace, SpanId: mustHex("3333333333333333"), Name: "tools/call \x1b[31mred",
				StartTimeUnixNano: 1_790_000_000_000_000_000, Attributes: []*common.KeyValue{kv(semconv.MCPMethodName, "tools/call")},
			},
		}}},
	}}}
}

func serve(t *testing.T, body []byte, ct, enc string) (*httptest.ResponseRecorder, []obs.Record) {
	t.Helper()
	var got []obs.Record
	rc := &Receiver{Offer: func(r obs.Record) bool { got = append(got, r); return true }}
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	if enc != "" {
		req.Header.Set("Content-Encoding", enc)
	}
	rr := httptest.NewRecorder()
	rc.ServeHTTP(rr, req)
	return rr, got
}

func TestProtobufFiltersAndMinimizes(t *testing.T) {
	b, _ := proto.Marshal(request())
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(b)
	zw.Close()
	rr, got := serve(t, gz.Bytes(), "application/x-protobuf", "gzip")
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if len(got) != 1 {
		t.Fatalf("accepted %d spans, want 1", len(got))
	}
	sp := got[0].Span
	if sp.Service != "demo-tools" || sp.Kind != obs.SpanKindServer || sp.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("unexpected span: %+v", sp)
	}
	for k := range sp.Attributes {
		if strings.Contains(k, "arguments") || strings.Contains(k, "result") {
			t.Errorf("sensitive attribute %q retained", k)
		}
	}
	var resp coltrace.ExportTraceServiceResponse
	if err := proto.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.GetPartialSuccess().GetRejectedSpans() != 2 {
		t.Errorf("rejected = %d, want 2", resp.GetPartialSuccess().GetRejectedSpans())
	}
}

func TestJSONEncoding(t *testing.T) {
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"demo-tools"}}]},
	"scopeSpans":[{"spans":[{"traceId":"4BF92F3577B34DA6A3CE929D0E0E4736","spanId":"00f067aa0ba902b7","parentSpanId":"",
	"name":"tools/call scale_workload","kind":2,"startTimeUnixNano":"1790000000000000000","endTimeUnixNano":1790000000200000000,
	"attributes":[{"key":"mcp.method.name","value":{"stringValue":"tools/call"}},{"key":"jsonrpc.request.id","value":{"intValue":"7"}}],
	"status":{"code":2}}]}]}]}`
	rr, got := serve(t, []byte(body), "application/json", "")
	if rr.Code != 200 || len(got) != 1 {
		t.Fatalf("status %d, spans %d: %s", rr.Code, len(got), rr.Body)
	}
	sp := got[0].Span
	if sp.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || !sp.Error || sp.Attributes[semconv.JSONRPCRequestID] != "7" {
		t.Errorf("unexpected span: %+v", sp)
	}
	if sp.End.Sub(sp.Start).Milliseconds() != 200 {
		t.Errorf("duration = %v", sp.End.Sub(sp.Start))
	}
}

func TestRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name, ct, enc string
		body          []byte
		want          int
	}{
		{"content type", "text/plain", "", []byte("x"), http.StatusUnsupportedMediaType},
		{"garbage proto", "application/x-protobuf", "", []byte{0xff, 0xff, 0xff}, http.StatusBadRequest},
		{"garbage json", "application/json", "", []byte("{"), http.StatusBadRequest},
		{"bad gzip", "application/json", "gzip", []byte("not gzip"), http.StatusBadRequest},
		{"encoding", "application/json", "br", []byte("{}"), http.StatusBadRequest},
		{"too large", "application/json", "", bytes.Repeat([]byte(" "), MaxBodyBytes+1), http.StatusBadRequest},
	}
	for _, c := range cases {
		rr, got := serve(t, c.body, c.ct, c.enc)
		if rr.Code != c.want || len(got) != 0 {
			t.Errorf("%s: status %d (want %d), spans %d", c.name, rr.Code, c.want, len(got))
		}
	}
}

func TestGzipBombIsBounded(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(bytes.Repeat([]byte("a"), MaxBodyBytes*2))
	zw.Close()
	rr, _ := serve(t, gz.Bytes(), "application/json", "gzip")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
}

func TestBackpressureReturns503(t *testing.T) {
	b, _ := proto.Marshal(request())
	rc := &Receiver{Offer: func(obs.Record) bool { return false }}
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/x-protobuf")
	rr := httptest.NewRecorder()
	rc.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q", rr.Code, rr.Header().Get("Retry-After"))
	}
}

func FuzzDecodeJSON(f *testing.F) {
	f.Add([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"00f067aa0ba902b7","name":"tools/call x","kind":2,"startTimeUnixNano":"1790000000000000000","attributes":[{"key":"mcp.method.name","value":{"stringValue":"tools/call"}}]}]}]}]}`))
	f.Add([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"startTimeUnixNano":18446744073709551615}]}]}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		spans, _, err := DecodeJSON(b)
		if err != nil {
			return
		}
		for _, sp := range spans {
			if !relevant(sp) {
				t.Fatal("irrelevant span returned")
			}
			for k := range sp.Attributes {
				if !allowedAttributes[k] {
					t.Fatalf("attribute %q not allowlisted", k)
				}
			}
		}
	})
}

func FuzzDecodeProto(f *testing.F) {
	b, _ := proto.Marshal(request())
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		spans, _, err := DecodeProto(b)
		if err != nil {
			return
		}
		for _, sp := range spans {
			for k := range sp.Attributes {
				if !allowedAttributes[k] {
					t.Fatalf("attribute %q not allowlisted", k)
				}
			}
		}
	})
}
