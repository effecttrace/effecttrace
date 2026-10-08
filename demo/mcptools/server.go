// Package mcptools is the demo MCP tool server used by the EffectTrace lab.
// It is a demo ACTOR with narrowly scoped mutation rights, not part of
// EffectTrace itself. It exposes a handful of Kubernetes operations as MCP
// tools and instruments them following the OpenTelemetry MCP semantic
// conventions (Development status):
//
//   - the W3C trace context is extracted from params._meta (SEP-414) and used
//     as the parent of a SERVER span named "tools/call {tool}";
//   - Kubernetes requests made by the tool run under that span through
//     k8sinstrument, so their client spans carry the Audit-ID and object
//     identity EffectTrace needs.
//
// Tool arguments and results are never recorded on spans.
package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/effecttrace/effecttrace/pkg/semconv"
)

// ProtocolVersionKey is the MCP _meta key carrying the protocol version.
const ProtocolVersionKey = "io.modelcontextprotocol/protocolVersion"

// Server holds tool dependencies.
type Server struct {
	Client            kubernetes.Interface
	Tracer            trace.Tracer
	Propagator        propagation.TextMapPropagator
	AllowedNamespaces []string
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
var dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

func (s *Server) check(ns string, names ...string) error {
	if !slices.Contains(s.AllowedNamespaces, ns) {
		return fmt.Errorf("namespace %q is not allowed for this demo actor", ns)
	}
	for _, n := range names {
		if !dnsSubdomain.MatchString(n) {
			return fmt.Errorf("invalid name %q", n)
		}
	}
	return nil
}

// metaCarrier adapts MCP _meta to an OpenTelemetry TextMapCarrier.
type metaCarrier map[string]any

func (m metaCarrier) Get(k string) string { v, _ := m[k].(string); return v }
func (m metaCarrier) Set(k, v string)     { m[k] = v }
func (m metaCarrier) Keys() []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Middleware creates the MCP server span for tools/call.
func (s *Server) Middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != semconv.MCPMethodToolsCall {
			return next(ctx, method, req)
		}
		tool := ""
		meta := map[string]any{}
		if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil {
			tool = p.Name
			if m := p.GetMeta(); m != nil {
				meta = m
			}
		}
		ctx = s.Propagator.Extract(ctx, metaCarrier(meta))
		attrs := []attribute.KeyValue{
			attribute.String(semconv.MCPMethodName, method),
			attribute.String(semconv.GenAIToolName, tool),
			attribute.String(semconv.GenAIOperationName, semconv.GenAIOperationExecuteTool),
			attribute.String(semconv.NetworkTransport, "tcp"),
			attribute.String(semconv.NetworkProtoName, "http"),
		}
		if v, ok := meta[ProtocolVersionKey].(string); ok && len(v) <= 32 {
			attrs = append(attrs, attribute.String(semconv.MCPProtocolVersion, v))
		}
		ctx, span := s.Tracer.Start(ctx, method+" "+tool, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		defer span.End()
		res, err := next(ctx, method, req)
		switch {
		case err != nil:
			span.SetAttributes(attribute.String(semconv.ErrorType, "_OTHER"))
			span.SetStatus(codes.Error, "tool call failed")
		default:
			if r, ok := res.(*mcp.CallToolResult); ok && r != nil && r.IsError {
				span.SetAttributes(attribute.String(semconv.ErrorType, semconv.ErrorTypeToolError))
				span.SetStatus(codes.Error, "tool returned an error")
			}
		}
		return res, err
	}
}

// Inputs.

type WorkloadIn struct {
	Namespace  string `json:"namespace" jsonschema:"namespace of the Deployment"`
	Deployment string `json:"deployment" jsonschema:"name of the Deployment"`
}

type ScaleIn struct {
	Namespace  string `json:"namespace" jsonschema:"namespace of the Deployment"`
	Deployment string `json:"deployment" jsonschema:"name of the Deployment"`
	Replicas   int32  `json:"replicas" jsonschema:"desired replica count (0-20)"`
}

type ImageIn struct {
	Namespace  string `json:"namespace" jsonschema:"namespace of the Deployment"`
	Deployment string `json:"deployment" jsonschema:"name of the Deployment"`
	Container  string `json:"container" jsonschema:"container name"`
	Image      string `json:"image" jsonschema:"new image reference"`
}

type PodIn struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the Pod"`
	Pod       string `json:"pod" jsonschema:"name of the Pod"`
}

type ConfigIn struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the ConfigMap"`
	ConfigMap string `json:"configmap" jsonschema:"name of the ConfigMap"`
	Key       string `json:"key" jsonschema:"data key"`
	Value     string `json:"value" jsonschema:"new value (synthetic demo data only)"`
}

type ResourcesIn struct {
	Namespace  string `json:"namespace" jsonschema:"namespace of the Deployment"`
	Deployment string `json:"deployment" jsonschema:"name of the Deployment"`
	Container  string `json:"container" jsonschema:"container name"`
	CPULimit   string `json:"cpu_limit" jsonschema:"CPU limit, for example 200m"`
	MemLimit   string `json:"memory_limit" jsonschema:"memory limit, for example 128Mi"`
}

type SelectorIn struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the Service"`
	Service   string `json:"service" jsonschema:"name of the Service"`
	Key       string `json:"key" jsonschema:"selector label key"`
	Value     string `json:"value" jsonschema:"selector label value"`
}

// Out is the structured result of every tool.
type Out struct {
	Message    string `json:"message"`
	UID        string `json:"uid,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

func result(msg string, out Out) (*mcp.CallToolResult, Out, error) {
	out.Message = msg
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, out, nil
}

func toolError(err error) (*mcp.CallToolResult, Out, error) {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, Out{}, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Register adds all tools to srv.
func (s *Server) Register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "restart_workload", Description: "Trigger a rolling restart of a Deployment (like kubectl rollout restart)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in WorkloadIn) (*mcp.CallToolResult, Out, error) {
			if err := s.check(in.Namespace, in.Deployment); err != nil {
				return toolError(err)
			}
			patch := mustJSON(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
				"annotations": map[string]string{"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format(time.RFC3339)}}}}})
			d, err := s.Client.AppsV1().Deployments(in.Namespace).Patch(ctx, in.Deployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: "effecttrace-demo-tools"})
			if err != nil {
				return toolError(err)
			}
			return result(fmt.Sprintf("restart requested for deployment %s/%s (generation %d)", in.Namespace, in.Deployment, d.Generation), Out{UID: string(d.UID), Generation: d.Generation})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "scale_workload", Description: "Set the replica count of a Deployment."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ScaleIn) (*mcp.CallToolResult, Out, error) {
			if err := s.check(in.Namespace, in.Deployment); err != nil {
				return toolError(err)
			}
			if in.Replicas < 0 || in.Replicas > 20 {
				return toolError(errors.New("replicas must be between 0 and 20"))
			}
			patch := mustJSON(map[string]any{"spec": map[string]any{"replicas": in.Replicas}})
			var sc autoscalingv1.Scale
			err := s.Client.AppsV1().RESTClient().Patch(types.MergePatchType).Namespace(in.Namespace).Resource("deployments").
				Name(in.Deployment).SubResource("scale").Param("fieldManager", "effecttrace-demo-tools").Body(patch).Do(ctx).Into(&sc)
			if err != nil {
				return toolError(err)
			}
			return result(fmt.Sprintf("scaled deployment %s/%s to %d replicas", in.Namespace, in.Deployment, in.Replicas), Out{UID: string(sc.UID)})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "update_image", Description: "Change the image of one container of a Deployment."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ImageIn) (*mcp.CallToolResult, Out, error) {
			if err := s.check(in.Namespace, in.Deployment); err != nil {
				return toolError(err)
			}
			if !dnsLabel.MatchString(in.Container) || len(in.Image) == 0 || len(in.Image) > 255 || strings.ContainsAny(in.Image, " \t\n") {
				return toolError(errors.New("invalid container or image"))
			}
			patch := mustJSON(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []map[string]string{{"name": in.Container, "image": in.Image}}}}}})
			d, err := s.Client.AppsV1().Deployments(in.Namespace).Patch(ctx, in.Deployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: "effecttrace-demo-tools"})
			if err != nil {
				return toolError(err)
			}
			return result(fmt.Sprintf("updated image of %s/%s container %s", in.Namespace, in.Deployment, in.Container), Out{UID: string(d.UID), Generation: d.Generation})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "delete_pod", Description: "Delete one Pod so that its controller replaces it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in PodIn) (*mcp.CallToolResult, Out, error) {
			if err := s.check(in.Namespace, in.Pod); err != nil {
				return toolError(err)
			}
			if err := s.Client.CoreV1().Pods(in.Namespace).Delete(ctx, in.Pod, metav1.DeleteOptions{}); err != nil {
				return toolError(err)
			}
			return result(fmt.Sprintf("deleted pod %s/%s", in.Namespace, in.Pod), Out{})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "set_config", Description: "Set one key of a ConfigMap (synthetic demo data)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ConfigIn) (*mcp.CallToolResult, Out, error) {
			if err := s.check(in.Namespace, in.ConfigMap); err != nil {
				return toolError(err)
			}
			if !regexp.MustCompile(`^[-._a-zA-Z0-9]{1,64}$`).MatchString(in.Key) || len(in.Value) > 256 {
				return toolError(errors.New("invalid key or value"))
			}
			patch := mustJSON(map[string]any{"data": map[string]string{in.Key: in.Value}})
			cm, err := s.Client.CoreV1().ConfigMaps(in.Namespace).Patch(ctx, in.ConfigMap, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "effecttrace-demo-tools"})
			if err != nil {
				return toolError(err)
			}
			return result(fmt.Sprintf("updated configmap %s/%s", in.Namespace, in.ConfigMap), Out{UID: string(cm.UID)})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "set_resources", Description: "Set CPU and memory limits of one container of a Deployment."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ResourcesIn) (*mcp.CallToolResult, Out, error) {
			if err := s.check(in.Namespace, in.Deployment); err != nil {
				return toolError(err)
			}
			qty := regexp.MustCompile(`^[0-9]{1,6}(m|Mi|Gi)?$`)
			if !dnsLabel.MatchString(in.Container) || !qty.MatchString(in.CPULimit) || !qty.MatchString(in.MemLimit) {
				return toolError(errors.New("invalid container or quantity"))
			}
			patch := mustJSON(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []map[string]any{{"name": in.Container, "resources": map[string]any{"limits": map[string]string{"cpu": in.CPULimit, "memory": in.MemLimit}}}}}}}})
			d, err := s.Client.AppsV1().Deployments(in.Namespace).Patch(ctx, in.Deployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: "effecttrace-demo-tools"})
			if err != nil {
				return toolError(err)
			}
			return result(fmt.Sprintf("updated resources of %s/%s container %s", in.Namespace, in.Deployment, in.Container), Out{UID: string(d.UID), Generation: d.Generation})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "update_service_selector", Description: "Set one label of a Service selector."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SelectorIn) (*mcp.CallToolResult, Out, error) {
			if err := s.check(in.Namespace, in.Service); err != nil {
				return toolError(err)
			}
			if !dnsSubdomain.MatchString(in.Key) && !regexp.MustCompile(`^[a-z0-9.-]+/[a-z0-9.-]+$`).MatchString(in.Key) || !dnsLabel.MatchString(in.Value) {
				return toolError(errors.New("invalid selector"))
			}
			patch := mustJSON(map[string]any{"spec": map[string]any{"selector": map[string]string{in.Key: in.Value}}})
			svc, err := s.Client.CoreV1().Services(in.Namespace).Patch(ctx, in.Service, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "effecttrace-demo-tools"})
			if err != nil {
				return toolError(err)
			}
			return result(fmt.Sprintf("updated selector of service %s/%s", in.Namespace, in.Service), Out{UID: string(svc.UID)})
		})
}
