package k8sinstrument

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/effecttrace/effecttrace/pkg/semconv"
)

func protobufDeployment(t testing.TB) []byte {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", UID: "8f1e-uid", ResourceVersion: "4242", Generation: 15}}
	raw, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	u := &runtime.Unknown{TypeMeta: runtime.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, Raw: raw, ContentType: "application/vnd.kubernetes.protobuf"}
	b, err := u.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte{0x6b, 0x38, 0x73, 0x00}, b...)
}

func TestParseProtobufMetadata(t *testing.T) {
	m, err := parseProtobufMetadata(protobufDeployment(t))
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != "Deployment" || m.Metadata.UID != "8f1e-uid" || m.Metadata.ResourceVersion != "4242" || m.Metadata.Generation != 15 {
		t.Fatalf("parsed %+v", m)
	}
}

func TestParseRequest(t *testing.T) {
	cases := []struct {
		method, path string
		want         RequestInfo
	}{
		{"PATCH", "/apis/apps/v1/namespaces/shop/deployments/checkout", RequestInfo{Verb: "patch", APIGroup: "apps", APIVersion: "v1", Namespace: "shop", Resource: "deployments", Name: "checkout"}},
		{"PATCH", "/apis/apps/v1/namespaces/shop/deployments/checkout/scale", RequestInfo{Verb: "patch", APIGroup: "apps", APIVersion: "v1", Namespace: "shop", Resource: "deployments", Name: "checkout", Subresource: "scale"}},
		{"DELETE", "/api/v1/namespaces/shop/pods/p-1", RequestInfo{Verb: "delete", APIVersion: "v1", Namespace: "shop", Resource: "pods", Name: "p-1"}},
		{"DELETE", "/api/v1/namespaces/shop/pods", RequestInfo{Verb: "deletecollection", APIVersion: "v1", Namespace: "shop", Resource: "pods"}},
		{"POST", "/api/v1/namespaces", RequestInfo{Verb: "create", APIVersion: "v1", Resource: "namespaces"}},
		{"PUT", "/api/v1/namespaces/shop", RequestInfo{Verb: "update", APIVersion: "v1", Resource: "namespaces", Name: "shop"}},
	}
	for _, c := range cases {
		got, ok := ParseRequest(c.method, c.path, false)
		if !ok || got != c.want {
			t.Errorf("%s %s = %+v, %v", c.method, c.path, got, ok)
		}
	}
	for _, p := range []string{"/version", "/openapi/v3", "/", "/apis/apps"} {
		if _, ok := ParseRequest("GET", p, false); ok {
			t.Errorf("non-resource path %s parsed", p)
		}
	}
}

func roundTrip(t *testing.T, ct string, body []byte) (map[string]string, []byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Audit-Id", "3f1c0e9a-0000-4000-8000-000000000001")
		w.Write(body)
	}))
	defer srv.Close()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	client := &http.Client{Transport: Wrap(http.DefaultTransport)}
	ctx, span := tp.Tracer("t").Start(context.Background(), "parent")
	_ = span
	req, _ := http.NewRequestWithContext(ctx, http.MethodPatch, srv.URL+"/apis/apps/v1/namespaces/shop/deployments/checkout", strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	span.End()
	// otelhttp uses the global provider; read attributes from the client span
	// through the recorder of the context's provider instead.
	attrs := map[string]string{}
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.String()
		}
	}
	return attrs, got
}

func TestTransportRecordsIdentifiersAndPreservesBody(t *testing.T) {
	jsonBody, _ := json.Marshal(map[string]any{"kind": "Deployment", "metadata": map[string]any{"uid": "u-json", "resourceVersion": "7", "generation": 3},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"env": []any{map[string]any{"name": "DB_PASSWORD", "value": "hunter2"}}}}}}}})
	for _, c := range []struct {
		ct   string
		body []byte
		uid  string
	}{
		{"application/json", jsonBody, "u-json"},
		{"application/vnd.kubernetes.protobuf", protobufDeployment(t), "8f1e-uid"},
	} {
		attrs, got := roundTrip(t, c.ct, c.body)
		if !bytes.Equal(got, c.body) {
			t.Errorf("%s: body altered", c.ct)
		}
		if attrs[semconv.K8sObjectUID] != c.uid || attrs[semconv.K8sAuditID] == "" || attrs[semconv.K8sVerb] != "patch" {
			t.Errorf("%s: attributes %v", c.ct, attrs)
		}
		for k, v := range attrs {
			if strings.Contains(v, "hunter2") {
				t.Errorf("%s: body content leaked into attribute %s", c.ct, k)
			}
		}
	}
	_ = attribute.String
}

func FuzzParseProtobufMetadata(f *testing.F) {
	f.Add(protobufDeployment(f))
	f.Add([]byte{0x6b, 0x38, 0x73, 0x00})
	f.Add([]byte{0x6b, 0x38, 0x73, 0x00, 0x0a, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parseProtobufMetadata(b)
	})
}

func FuzzParseRequest(f *testing.F) {
	f.Add("PATCH", "/apis/apps/v1/namespaces/shop/deployments/checkout/scale")
	f.Add("DELETE", "/api/v1/namespaces//pods/")
	f.Fuzz(func(t *testing.T, method, path string) {
		info, ok := ParseRequest(method, path, false)
		if ok && (len(info.Name) > 253 || len(info.Namespace) > 253) {
			t.Fatal("oversized name accepted")
		}
	})
}
