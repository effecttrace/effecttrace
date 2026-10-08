# Semantic conventions

This page lists every attribute EffectTrace reads or writes. Keys are defined
in [`pkg/semconv/semconv.go`](../pkg/semconv/semconv.go). Project-specific
keys use the `effecttrace.*` namespace. Keys from OpenTelemetry semantic
conventions are repeated as constants because the GenAI and MCP conventions
have Development status and are no longer generated in the newest Go
`semconv` packages.

All values are strings unless the type column says otherwise.

## OpenTelemetry attributes read by EffectTrace

| Key | Status upstream | Read on | Use |
| --- | --- | --- | --- |
| `mcp.method.name` | Development | SERVER spans | `tools/call` marks an action |
| `mcp.protocol.version` | Development | SERVER spans | retained, informational |
| `gen_ai.tool.name` | Development | SERVER spans | tool name of the action |
| `gen_ai.operation.name` | Development | SERVER spans | retained (`execute_tool`) |
| `jsonrpc.request.id` | Development | SERVER spans | retained, informational |
| `error.type` | Stable | SERVER spans | non-empty marks the action outcome as `error` |
| `http.request.method` | Stable | client spans | retained |
| `http.response.status_code` | Stable | client spans | request status when no audit event confirms it; a request with unknown status is never treated as a successful mutation |
| `service.name` | Stable (resource) | resources | span service; the parent span's service becomes the actor `mcp-client:<service>` |

`gen_ai.tool.call.arguments` and `gen_ai.tool.call.result` (opt-in upstream)
are never retained. No other attribute survives ingest.

## `effecttrace.k8s.*`: request attributes on Kubernetes client spans

Written by [`pkg/k8sinstrument`](../pkg/k8sinstrument) on client spans of
mutating requests and read by the OTLP receiver. They describe the **target**
of an API request. That is why they do not reuse the `k8s.*` resource
attributes, which describe the entity producing telemetry.

| Key | Type | Set when | Cardinality notes |
| --- | --- | --- | --- |
| `effecttrace.k8s.verb` | string | always (mutating requests only): `create`, `update`, `patch`, `delete`, `deletecollection` | bounded |
| `effecttrace.k8s.api_group` | string | non-core group | bounded by installed APIs |
| `effecttrace.k8s.resource` | string | always | bounded by installed APIs |
| `effecttrace.k8s.subresource` | string | subresource request (for example `scale`) | bounded |
| `effecttrace.k8s.object.namespace` | string | namespaced request | per namespace |
| `effecttrace.k8s.object.name` | string | named request | per object; unbounded over time |
| `effecttrace.k8s.audit_id` | string | the response carried an `Audit-Id` header of at most 128 bytes | unique per request |
| `effecttrace.k8s.object.kind` | string | 2xx JSON or protobuf response with a `kind` other than `Status` | bounded |
| `effecttrace.k8s.object.uid` | string | 2xx response with `metadata.uid` (at most 128 bytes) | unique per object |
| `effecttrace.k8s.object.resource_version` | string | 2xx response with `metadata.resourceVersion` (at most 128 bytes) | unique per write |
| `effecttrace.k8s.object.generation` | string (decimal integer) | 2xx response with `metadata.generation` > 0 | per write |

These attributes are span attributes, where high-cardinality identifiers are
normal. Do not copy them into metric labels.

## `effecttrace.*`: attributes on exported effect-graph spans

Written by [`internal/otelexport`](../internal/otelexport) on the
`effecttrace effect_graph` span and its `effecttrace.edge` events. See
[OpenTelemetry](opentelemetry.md#exporting-effect-graphs).

| Key | Type | Where | Values / notes |
| --- | --- | --- | --- |
| `effecttrace.graph.id` | string | span | `g-<action ID>`; unique per graph |
| `effecttrace.graph.status` | string | span | `COMPLETE` (only complete graphs are exported) |
| `effecttrace.action.id` | string | span | unique per action |
| `effecttrace.action.kind` | string | span | `MCP_TOOL_CALL`, `KUBERNETES_API_CALL` |
| `effecttrace.edges.direct` | int | span | count of `DIRECT` edges |
| `effecttrace.edges.trace_link` | int | span | count of `TRACE_LINK` edges |
| `effecttrace.edges.structural` | int | span | count of `STRUCTURAL` edges |
| `effecttrace.edges.event_reference` | int | span | count of `EVENT_REFERENCE` edges |
| `effecttrace.edges.temporal_correlation` | int | span | count of `TEMPORAL_CORRELATION` edges |
| `effecttrace.edges.inferred` | int | span | always 0 in v0.1 |
| `effecttrace.edges.truncated` | int | span | number of edges not exported as events (beyond 128) |
| `effecttrace.link.type` | string | span link | `initiating_action` |
| `effecttrace.edge.relationship` | string | edge event | one of the six relationships |
| `effecttrace.evidence.type` | string | edge event | one of the evidence classes |
| `effecttrace.edge.from` | string | edge event | node ID |
| `effecttrace.edge.to` | string | edge event | node ID |
| `effecttrace.edge.reason` | string | edge event | rule-generated sentence, at most 256 bytes |
| `effecttrace.node.type` | string | edge event | node type of the edge target |
| `effecttrace.k8s.uid` | string | edge event | UID of the target object, when it has one |

The `effecttrace.edges.*` keys are formed from the prefix `effecttrace.edges.`
and the lowercase evidence class.

### Reserved keys

These keys are defined in `pkg/semconv` but not emitted in v0.1:

| Key | Intended use |
| --- | --- |
| `effecttrace.node.id` | node ID on a future per-node representation |
| `effecttrace.node.grade` | path grade of a node |
| `effecttrace.observation.window.name` | observation window name |
| `effecttrace.observation.window.start` | observation window start |
| `effecttrace.observation.window.end` | observation window end |

## Names EffectTrace does not use

- `otel.*` is reserved by OpenTelemetry and never written.
- `k8s.*` resource attributes (for example `k8s.pod.uid`) describe the
  telemetry producer and are not used to describe request targets.
- There are no upstream semantic conventions for Kubernetes audit events;
  EffectTrace does not invent `k8s.audit.*` keys.

## Span names

| Span | Name | Kind | Producer |
| --- | --- | --- | --- |
| MCP client call | `tools/call {tool}` | CLIENT | MCP client (demo agent) |
| MCP tool execution | `tools/call {tool}` | SERVER | MCP server (demo tool server) |
| Kubernetes request | `{verb} {resource}` or `{verb} {resource}/{subresource}`, otherwise `HTTP {method}` | CLIENT | `pkg/k8sinstrument` |
| Effect graph | `effecttrace effect_graph` | INTERNAL | EffectTrace exporter |

## Metrics labels

EffectTrace's own Prometheus metrics use only bounded labels. Graph IDs, trace
IDs, span IDs and object UIDs are never labels. See
[Prometheus](prometheus.md#effecttraces-own-metrics).
