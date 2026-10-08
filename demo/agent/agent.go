// Package agent is a deterministic, scripted MCP client used by the
// EffectTrace demo and experiments. It does not use a language model: every
// tool call is chosen by the script, which makes runs reproducible.
//
// Each call creates an MCP CLIENT span "tools/call {tool}" and propagates
// its W3C trace context in params._meta (traceparent, tracestate), as
// specified by MCP SEP-414 and the OpenTelemetry MCP semantic conventions.
package agent

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/effecttrace/effecttrace/pkg/semconv"
)

// Agent calls tools on one MCP server.
type Agent struct {
	Endpoint   string
	Tracer     trace.Tracer
	Propagator propagation.TextMapPropagator
	// DropContext disables trace propagation (experiments of missing
	// context between the agent and the tool server).
	DropContext bool
}

// Result is the outcome of one call.
type Result struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
	Tool    string `json:"tool"`
	Text    string `json:"text"`
	IsError bool   `json:"isError"`
}

type carrier map[string]any

func (c carrier) Get(k string) string { v, _ := c[k].(string); return v }
func (c carrier) Set(k, v string)     { c[k] = v }
func (c carrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// Call invokes one tool and returns its result and trace identifiers.
func (a *Agent) Call(ctx context.Context, tool string, args map[string]any) (Result, error) {
	u, err := url.Parse(a.Endpoint)
	if err != nil {
		return Result{}, err
	}
	attrs := []attribute.KeyValue{
		attribute.String(semconv.MCPMethodName, semconv.MCPMethodToolsCall),
		attribute.String(semconv.GenAIToolName, tool),
		attribute.String(semconv.GenAIOperationName, semconv.GenAIOperationExecuteTool),
		attribute.String(semconv.NetworkTransport, "tcp"),
		attribute.String(semconv.NetworkProtoName, "http"),
	}
	if host, port, err := net.SplitHostPort(u.Host); err == nil {
		attrs = append(attrs, attribute.String("server.address", host))
		if p, err := strconv.Atoi(port); err == nil {
			attrs = append(attrs, attribute.Int("server.port", p))
		}
	}
	ctx, span := a.Tracer.Start(ctx, semconv.MCPMethodToolsCall+" "+tool, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()
	res := Result{Tool: tool, TraceID: span.SpanContext().TraceID().String(), SpanID: span.SpanContext().SpanID().String()}

	client := mcp.NewClient(&mcp.Implementation{Name: "effecttrace-demo-agent", Version: "v0.1.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: a.Endpoint, DisableStandaloneSSE: true}, nil)
	if err != nil {
		span.SetStatus(codes.Error, "connect failed")
		span.SetAttributes(attribute.String(semconv.ErrorType, "connect"))
		return res, err
	}
	defer func() { _ = cs.Close() }()
	meta := carrier{}
	if !a.DropContext {
		a.Propagator.Inject(ctx, meta)
	}
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: mcp.Meta(meta)})
	if err != nil {
		span.SetStatus(codes.Error, "call failed")
		span.SetAttributes(attribute.String(semconv.ErrorType, "_OTHER"))
		return res, err
	}
	res.IsError = out.IsError
	for _, c := range out.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			res.Text = t.Text
			break
		}
	}
	if out.IsError {
		span.SetStatus(codes.Error, "tool error")
		span.SetAttributes(attribute.String(semconv.ErrorType, semconv.ErrorTypeToolError))
		return res, errors.New("tool error: " + res.Text)
	}
	return res, nil
}
