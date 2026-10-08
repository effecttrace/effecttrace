# MCP

EffectTrace recognizes Model Context Protocol (MCP) tool calls as actions when
they are observed as OpenTelemetry spans. It does not proxy, gate or inspect
MCP traffic, and it is not an MCP gateway. Any MCP server that emits a SERVER
span for `tools/call` following the OpenTelemetry MCP semantic conventions,
and whose Kubernetes client is instrumented with
[`pkg/k8sinstrument`](../pkg/k8sinstrument), can be investigated.

## What EffectTrace needs from an MCP server

| Requirement | Why |
| --- | --- |
| A SERVER span named `tools/call {tool}` with `mcp.method.name = tools/call` | discovers the action |
| `gen_ai.tool.name` | names the tool in the graph |
| The W3C trace context of the caller extracted from `params._meta` and used as the parent | ties the tool call to the agent's trace and records the agent's service as actor |
| Kubernetes requests made under the tool span through an instrumented client | gives `TRACE_LINK` and `DIRECT` evidence |
| `error.type` (`tool_error` when the result has `isError: true`) and span status | records the outcome |
| Spans exported over OTLP to EffectTrace (directly or through an OpenTelemetry Collector) | ingest |

Tool arguments and results are not needed and are never retained (see
[OpenTelemetry](opentelemetry.md#which-spans-are-kept)).

## Trace context propagation

MCP propagates W3C trace context in the request's `params._meta` object. SEP-414
reserves the unprefixed `_meta` keys `traceparent`, `tracestate` and `baggage`
for this, using the W3C formats. The current MCP specification revision is
2026-07-28.

The client injects context into `_meta`; the server extracts it and uses it as
the parent of its SERVER span:

```json
{
  "jsonrpc": "2.0",
  "id": 7,
  "method": "tools/call",
  "params": {
    "name": "restart_workload",
    "arguments": { "namespace": "shop", "deployment": "checkout" },
    "_meta": {
      "traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
    }
  }
}
```

The OpenTelemetry semantic conventions for MCP are maintained in
[open-telemetry/semantic-conventions-genai](https://github.com/open-telemetry/semantic-conventions-genai)
and have **Development** status. They may change. The newest generated Go
`semconv` packages no longer contain the GenAI and MCP keys, so EffectTrace
defines the keys it uses as constants in [`pkg/semconv`](../pkg/semconv).

## The demo tool server

The lab's MCP tool server ([`demo/mcptools`](../demo/mcptools),
[`demo/cmd/mcp-tools`](../demo/cmd/mcp-tools)) is a demo **actor**, not part of
EffectTrace. It is built with the MCP Go SDK, serves Streamable HTTP at
`/mcp` in stateless mode with JSON responses and a 1 MiB request body limit,
and exposes these tools:

| Tool | Kubernetes request |
| --- | --- |
| `restart_workload` | strategic-merge PATCH of a Deployment's pod template `kubectl.kubernetes.io/restartedAt` annotation |
| `scale_workload` | merge PATCH of the Deployment `scale` subresource (0 to 20 replicas) |
| `update_image` | strategic-merge PATCH of one container image |
| `set_resources` | strategic-merge PATCH of one container's CPU and memory limits |
| `delete_pod` | DELETE of one Pod |
| `set_config` | merge PATCH of one ConfigMap key (synthetic data) |
| `update_service_selector` | merge PATCH of one Service selector label |

Every tool validates its inputs (DNS-1123 names, bounded values) and refuses
namespaces outside `ALLOWED_NAMESPACES` (the lab allows only `shop`). Its RBAC
is described in the [security model](security-model.md#demo-actor-rbac).

### Middleware

The server registers a receiving middleware that, for `tools/call` only:

1. extracts the trace context from `params._meta` with the W3C trace context
   and baggage propagators;
2. starts a SERVER span named `tools/call {tool}` with `mcp.method.name`,
   `gen_ai.tool.name`, `gen_ai.operation.name = execute_tool`,
   `network.transport = tcp`, `network.protocol.name = http`, and
   `mcp.protocol.version` when `_meta` carries
   `io.modelcontextprotocol/protocolVersion` (at most 32 bytes);
3. runs the tool, whose Kubernetes client is wrapped with
   `k8sinstrument.Wrap`, so its client spans are children of the tool span;
4. on a handler error sets `error.type = _OTHER` and an error status; on a
   result with `isError: true` sets `error.type = tool_error` and an error
   status.

Arguments and results are never set as span attributes. Baggage is extracted
into the context but not recorded. The server does not create a separate span
for the underlying HTTP transport.

Setting `UNINSTRUMENTED_KUBERNETES_CLIENT=true` disables the client wrapper;
lab experiment L24 uses it to show the degraded result.

### The scripted agent

The demo agent ([`demo/agent`](../demo/agent), [`demo/cmd/agent`](../demo/cmd/agent))
is a deterministic MCP client. **No language model is involved**: it calls
exactly the tool and arguments it is given. For each call it starts a CLIENT
span `tools/call {tool}` (with `server.address` and `server.port`), injects
`traceparent`, `tracestate` and `baggage` into `params._meta`, and prints the
trace ID so the graph can be found with `effecttrace explain --trace-id`.
`--no-trace-context` disables injection (lab experiment L23).

```sh
./bin/agent --endpoint http://127.0.0.1:18081/mcp restart_workload namespace=shop deployment=checkout
```

## Instrumenting your own MCP server

1. Extract `_meta` trace context and start the SERVER span as above. With the
   MCP Go SDK this is a receiving middleware (`AddReceivingMiddleware`); the
   SDK has no built-in OpenTelemetry support.
2. Wrap the Kubernetes client: `cfg.Wrap(k8sinstrument.Wrap)` on the
   `rest.Config` before creating the clientset, and pass the request context
   (which carries the tool span) to every client call.
3. Export spans over OTLP to an OpenTelemetry Collector that forwards them to
   EffectTrace.
4. Give the server the narrowest RBAC its tools need. EffectTrace observes; it
   does not authorize ([security model](security-model.md)).

## Related documents

- [OpenTelemetry](opentelemetry.md)
- [Semantic conventions](semantic-conventions.md)
- [Demo](demo.md)
