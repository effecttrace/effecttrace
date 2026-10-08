// Package k8sinstrument instruments Kubernetes API clients so that the
// client span of a mutating request carries the identifiers EffectTrace needs
// to connect an action to the object it changed:
//
//   - the Audit-ID that kube-apiserver returns in the response header, which
//     names the matching audit event, and
//   - the UID, resourceVersion and generation from the response object's
//     metadata.
//
// Request and response bodies are never recorded. Only the metadata fields
// listed above are read, from JSON or Kubernetes protobuf responses of at
// most MaxPeekBytes; the full body is passed on to the client unchanged.
//
// The package never sets the Audit-ID request header. kube-apiserver accepts
// client-supplied values without validation, which is undocumented behaviour;
// EffectTrace relies only on the server-generated value echoed back.
//
// Use it with client-go:
//
//	cfg.Wrap(k8sinstrument.Wrap)
package k8sinstrument

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/effecttrace/effecttrace/pkg/semconv"
)

// MaxPeekBytes bounds how much of a response body is read to extract object
// metadata. Larger responses are passed through without metadata.
const MaxPeekBytes = 4 << 20

// AuditIDHeader is the kube-apiserver response header naming the audit event.
const AuditIDHeader = "Audit-Id"

// Wrap returns rt wrapped with an OpenTelemetry client span per request and
// EffectTrace request attributes. It matches client-go's WrapperFunc.
func Wrap(rt http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(&Transport{Next: rt},
		otelhttp.WithSpanNameFormatter(spanName))
}

func spanName(_ string, r *http.Request) string {
	info, ok := ParseRequest(r.Method, r.URL.Path, r.URL.Query().Get("watch") == "true")
	if !ok {
		return "HTTP " + r.Method
	}
	if info.Subresource != "" {
		return info.Verb + " " + info.Resource + "/" + info.Subresource
	}
	return info.Verb + " " + info.Resource
}

// Transport records EffectTrace request attributes on the span found in the
// request context. Wrap it inside an otelhttp transport, as Wrap does.
type Transport struct {
	Next http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}
	info, ok := ParseRequest(req.Method, req.URL.Path, req.URL.Query().Get("watch") == "true")
	if !ok || !info.Mutating() {
		return next.RoundTrip(req)
	}
	span := trace.SpanFromContext(req.Context())
	attrs := []attribute.KeyValue{
		attribute.String(semconv.K8sVerb, info.Verb),
		attribute.String(semconv.K8sResource, info.Resource),
	}
	if info.APIGroup != "" {
		attrs = append(attrs, attribute.String(semconv.K8sAPIGroup, info.APIGroup))
	}
	if info.Subresource != "" {
		attrs = append(attrs, attribute.String(semconv.K8sSubresource, info.Subresource))
	}
	if info.Namespace != "" {
		attrs = append(attrs, attribute.String(semconv.K8sObjectNamespace, info.Namespace))
	}
	if info.Name != "" {
		attrs = append(attrs, attribute.String(semconv.K8sObjectName, info.Name))
	}
	span.SetAttributes(attrs...)

	resp, err := next.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if id := resp.Header.Get(AuditIDHeader); id != "" && len(id) <= 128 {
		span.SetAttributes(attribute.String(semconv.K8sAuditID, id))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.Body != nil {
		if format := encoding(resp.Header.Get("Content-Type")); format != "" && resp.Header.Get("Content-Encoding") == "" {
			meta, body, perr := peekMetadata(resp.Body, format)
			resp.Body = body
			if perr == nil {
				span.SetAttributes(meta.attributes()...)
			}
		}
	}
	return resp, nil
}

// encoding returns "json" or "protobuf" for supported response media types.
// client-go generated clients negotiate protobuf for built-in types and fall
// back to JSON. Other encodings (for example CBOR) are passed through
// without metadata.
func encoding(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	switch mt {
	case "application/json":
		return "json"
	case "application/vnd.kubernetes.protobuf":
		return "protobuf"
	}
	return ""
}

type objectMeta struct {
	Kind     string `json:"kind"`
	Metadata struct {
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
		Generation      int64  `json:"generation"`
	} `json:"metadata"`
}

func (m objectMeta) attributes() []attribute.KeyValue {
	var out []attribute.KeyValue
	// A Status response (for example from DELETE with propagation) has no UID.
	if m.Kind != "" && m.Kind != "Status" && len(m.Kind) <= 128 {
		out = append(out, attribute.String(semconv.K8sObjectKind, m.Kind))
	}
	if u := m.Metadata.UID; u != "" && len(u) <= 128 {
		out = append(out, attribute.String(semconv.K8sObjectUID, u))
	}
	if rv := m.Metadata.ResourceVersion; rv != "" && len(rv) <= 128 {
		out = append(out, attribute.String(semconv.K8sObjectResourceVersion, rv))
	}
	if m.Metadata.Generation > 0 {
		out = append(out, attribute.String(semconv.K8sObjectGeneration, strconv.FormatInt(m.Metadata.Generation, 10)))
	}
	return out
}

// peekMetadata reads up to MaxPeekBytes+1 of body, decodes object metadata if
// the whole body fit, and returns a reader that replays everything read
// followed by the unread remainder.
func peekMetadata(body io.ReadCloser, format string) (objectMeta, io.ReadCloser, error) {
	buf, err := io.ReadAll(io.LimitReader(body, MaxPeekBytes+1))
	replay := readCloser{Reader: io.MultiReader(bytes.NewReader(buf), body), Closer: body}
	if err != nil {
		return objectMeta{}, replay, err
	}
	if len(buf) > MaxPeekBytes {
		return objectMeta{}, replay, io.ErrShortBuffer
	}
	if format == "protobuf" {
		m, err := parseProtobufMetadata(buf)
		return m, replay, err
	}
	var m objectMeta
	if err := json.Unmarshal(buf, &m); err != nil {
		return objectMeta{}, replay, err
	}
	return m, replay, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}
