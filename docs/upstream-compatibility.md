# Upstream compatibility

EffectTrace composes upstream projects and depends on specific behaviour of
each. This page lists the versions the code and the lab are built and tested
with, and the upstream behaviour EffectTrace relies on. Versions were checked
in October 2026.

"Tested" means the version is used by `go.mod` or by the kind lab in which the
experiments run. Other versions may work but are not tested.

## Versions

| Component | Version | Where |
| --- | --- | --- |
| Go | 1.27.1 (`go` directive) | `go.mod` |
| Kubernetes (lab) | v1.37.0 (`kindest/node:v1.37.0`, default image of kind v0.33.0, pinned by digest) | `deploy/kind/kind-config.yaml.tmpl` |
| kind | v0.33.0 | `scripts/lab.sh` |
| `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` | v0.37.1 | `go.mod` |
| OpenTelemetry Go (`otel`, `sdk`, `trace`, `otlptracehttp`) | v1.47.0 | `go.mod` |
| `otelhttp` | v0.72.0 | `go.mod` |
| `go.opentelemetry.io/proto/otlp` | v1.11.1 | `go.mod` |
| OpenTelemetry Collector (lab) | 0.162.0, core distribution (`otel/opentelemetry-collector`), pinned by digest | `deploy/lab/observability.yaml` |
| Prometheus (lab) | v3.15.0 (`prom/prometheus`), pinned by digest | `deploy/lab/observability.yaml` |
| `prometheus/client_golang` | v1.24.1 | `go.mod` |
| MCP Go SDK | v1.8.0 | `go.mod` |
| MCP specification | 2026-07-28 (negotiated by the SDK) | demo agent and tool server |
| golangci-lint | v2.14.0 | `Makefile` |
| govulncheck | v1.8.0 | `Makefile` |

At the time of writing, the supported Kubernetes minor releases are 1.37,
1.36, 1.35 and 1.34 (1.34 reaches end of life on 2026-10-27). Only 1.37 is
exercised by the lab.

## Kubernetes behaviour relied on

| Behaviour | How EffectTrace uses it | Notes |
| --- | --- | --- |
| kube-apiserver returns an `Audit-Id` response header for ordinary requests | `k8sinstrument` records it on the client span | Not returned for proxied exec, attach or port-forward streams, which EffectTrace ignores anyway. The header is undocumented on kubernetes.io. |
| kube-apiserver accepts a client-supplied `Audit-ID` header without validation | EffectTrace never sets it and requires audit/span agreement | kubernetes/kubernetes [#127801](https://github.com/kubernetes/kubernetes/issues/127801), [#101597](https://github.com/kubernetes/kubernetes/issues/101597) |
| `objectRef.uid` is filled only from a decoded request object | audit-only actions resolve the UID by name at request time | empty for PATCH and DELETE at every audit level |
| Audit log backend writes `audit.k8s.io/v1` events as JSON lines and rotates by size | the tailer reads `ResponseComplete` events and follows rotation | the webhook backend is not supported |
| Controller `ownerReferences` with UIDs | structural scope | non-controller owners are ignored |
| `metadata.generation` increases on spec changes of workload kinds; status updates do not change it | no-op detection | |
| Deployment `status.observedGeneration`, `updatedReplicas`, `availableReplicas`, `replicas` | stable-status detection | conditions are not read |
| `deployment.kubernetes.io/revision` annotation, `pod-template-hash` label | displayed revision | |
| `kubectl.kubernetes.io/restartedAt` pod template annotation | how `kubectl rollout restart` and the demo restart tool trigger a rollout | |
| Deployment and ReplicaSet controllers emit core/v1 Events (`ScalingReplicaSet`, `SuccessfulCreate`, `SuccessfulDelete`) with `source.component` set and `eventTime` often empty | EVENT_REFERENCE evidence; the Event time is the collector's observation time for live Events | Events written through `events.k8s.io/v1` (for example by the scheduler) are served by the same core/v1 API |
| resourceVersion | compared for equality only | KEP-5504 documents orderable resourceVersions for built-in types from 1.35; the `ObjectMeta` documentation still calls them opaque |
| client-go negotiates Kubernetes protobuf for built-in types | `k8sinstrument` decodes protobuf metadata without decoding objects | CBOR responses would pass through without metadata |
| API server tracing (GA since 1.34) | not used | incoming `traceparent` is a parent only for privileged callers; audit events carry no trace ID |
| Informer transforms (`SetTransform` / `WithTransform`) | data minimization before caching | |

## OpenTelemetry behaviour relied on

| Behaviour | Notes |
| --- | --- |
| OTLP/HTTP `POST /v1/traces` on port 4318 with protobuf or JSON | OTLP/JSON uses hex IDs, integer enums, 64-bit integers as strings |
| Exporters retry on 503 with `Retry-After` | EffectTrace's backpressure signal |
| Span links for asynchronous relationships | graph export uses a link to the tool span, not a parent |
| MCP semantic conventions: span name `{mcp.method.name} {target}`, `mcp.method.name` required, `gen_ai.tool.name`, `error.type = tool_error`, `gen_ai.operation.name = execute_tool`, arguments and results opt-in | Development status, maintained in [open-telemetry/semantic-conventions-genai](https://github.com/open-telemetry/semantic-conventions-genai) |
| Go `semconv` packages | the newest generated package (v1.43.0) has Kubernetes and JSON-RPC keys but no GenAI or MCP keys (last in v1.41.0); EffectTrace defines them in `pkg/semconv` |
| Semantic conventions core release | v1.44.0 |
| Collector exporter name `otlp_http` | the `otlphttp` alias is deprecated |
| `otel.*` attribute namespace reserved | never written by EffectTrace |

## MCP behaviour relied on

| Behaviour | Notes |
| --- | --- |
| `params._meta` carries `traceparent`, `tracestate`, `baggage` (unprefixed, W3C formats) | SEP-414 (Final) |
| `_meta["io.modelcontextprotocol/protocolVersion"]` | recorded as `mcp.protocol.version` by the demo server |
| Stateless Streamable HTTP | the demo server sets `Stateless: true` for the 2026-07-28 revision |
| The MCP Go SDK has no built-in OpenTelemetry support | the demo uses a receiving middleware |

## Prometheus behaviour relied on

| Behaviour | Notes |
| --- | --- |
| `GET /api/v1/query_range` returning `matrix` results with `[timestamp, "value"]` pairs | any Prometheus-compatible API should work; only Prometheus v3.15.0 is tested |

## When upstream changes

- **MCP or GenAI semantic conventions change**: update `pkg/semconv`, the OTLP
  allowlist and [semantic conventions](semantic-conventions.md); keep reading
  old keys for a transition period.
- **Kubernetes starts validating client Audit-IDs**: no change needed;
  agreement checks remain.
- **Kubernetes adds `objectRef.uid` for PATCH**: the audit UID rule would be
  used more often, reducing reliance on name resolution.
- **client-go switches built-in types to CBOR**: add CBOR metadata decoding to
  `k8sinstrument` ([roadmap](../ROADMAP.md)).
