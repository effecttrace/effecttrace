# HTTP API

The collector serves a read-only HTTP API on `--api-listen` (default `:8080`).
There are no write endpoints. Routes are implemented in
[`internal/api/api.go`](../internal/api/api.go).

## Authentication

When the collector is started with `--api-token-file`, every `/api/*` route
requires:

```http
Authorization: Bearer <token>
```

The token is compared in constant time. A missing or wrong token returns
`401` with `WWW-Authenticate: Bearer`. `/healthz`, `/readyz` and `/metrics` are
never authenticated. There is no built-in TLS or per-user authorization; see
the [security model](security-model.md#network-exposure).

## Common headers

Every response carries:

```http
X-Content-Type-Options: nosniff
Cache-Control: no-store
Content-Security-Policy: default-src 'none'
```

JSON responses are `application/json`, indented, and HTML-escaped (`<`, `>`
and `&` are encoded as `<`, `>` and `&`).

## Routes

| Method and path | Auth | Response |
| --- | --- | --- |
| `GET /healthz` | no | `ok` |
| `GET /readyz` | no | `ok`, or `503 not ready` until Kubernetes informers have synced |
| `GET /metrics` | no | Prometheus exposition ([Prometheus](prometheus.md#effecttraces-own-metrics)) |
| `GET /api/v1/status` | yes | version, source health, retained record counts |
| `GET /api/v1/actions` | yes | action summaries |
| `GET /api/v1/effects/{id}` | yes | canonical JSON effect graph |
| `GET /api/v1/effects/{id}/graph` | yes | rendered graph (text, DOT or Mermaid) |
| `GET /api/v1/traces/{traceID}/actions` | yes | action IDs recorded in a trace |

Only `GET` is routed; other methods on these paths return `405`.

### GET /api/v1/status

```json
{
  "version": "v0.1.0 (0123456789ab)",
  "sources": [
    { "source": "kubernetes-audit", "healthy": true, "detail": "tailing audit log", "lastReport": "...", "startedAt": "..." },
    { "source": "kubernetes-watch", "healthy": true, "detail": "informers synced", "lastReport": "...", "startedAt": "..." },
    { "source": "kubernetes-events", "healthy": true, "detail": "informers synced", "lastReport": "...", "startedAt": "..." },
    { "source": "otlp", "healthy": true, "detail": "receiver listening", "lastReport": "...", "startedAt": "..." },
    { "source": "prometheus", "healthy": false, "detail": "not reported", "lastReport": "0001-01-01T00:00:00Z" }
  ],
  "counts": { "requests": 0, "spans": 0, "objects": 0, "events": 0, "metrics": 0 }
}
```

Sources are always listed in this order. A source that never reported shows
`healthy: false` and `detail: "not reported"`. The Prometheus source reports
only after its first evaluation. Values shown are placeholders.

### GET /api/v1/actions

Query parameters:

| Parameter | Default | Validation |
| --- | --- | --- |
| `since` | all retained actions | RFC 3339 timestamp (nanoseconds allowed) |
| `limit` | `200` | integer 1 to 1000; the **most recent** `limit` actions are returned |

Response, oldest first:

```json
{
  "actions": [
    {
      "id": "mcp-4bf92f3577b34da6-00f067aa0ba902b7",
      "kind": "MCP_TOOL_CALL",
      "name": "tools/call restart_workload",
      "actor": "mcp-client:demo-agent",
      "startedAt": "...",
      "traceId": "4bf92f3577b34da6a3ce929d0e0e4736",
      "status": "COMPLETE",
      "edges": { "DIRECT": 1, "TRACE_LINK": 1, "STRUCTURAL": 1, "EVENT_REFERENCE": 1 },
      "ambiguities": 0
    }
  ]
}
```

`edges` counts edges by evidence class; classes with no edges are omitted.
Identifiers and counts in the example are illustrative.

### GET /api/v1/effects/{id}

Returns the canonical JSON effect graph of action `{id}`, validated before it
is written. The format is described in [effect graph](effect-graph.md) and
[`schemas/effect-graph.schema.json`](../schemas/effect-graph.schema.json).
`{id}` must match `^[A-Za-z0-9._:-]{1,200}$`.

### GET /api/v1/effects/{id}/graph

| Parameter | Values | Content type |
| --- | --- | --- |
| `format` | `text` (default) | `text/plain; charset=utf-8` (the `explain` view, without color) |
| | `dot` | `text/vnd.graphviz; charset=utf-8` |
| | `mermaid` | `text/plain; charset=utf-8` |
| `verbose` | `true` | text only: every node and every edge reason |

### GET /api/v1/traces/{traceID}/actions

`{traceID}` is 32 hex digits (case-insensitive; normalized to lowercase).
Returns `{"actions": ["<action id>", ...]}`, empty when the trace holds no
tool call.

## Errors

Errors are JSON objects of the form `{"error": "<message>"}`.

| Status | Message | Cause |
| --- | --- | --- |
| `400` | `since must be RFC 3339` | invalid `since` |
| `400` | `limit must be 1..1000` | invalid `limit` |
| `400` | `invalid action id` | `{id}` does not match the pattern |
| `400` | `invalid trace id` | `{traceID}` is not 32 hex digits |
| `400` | `format must be text, dot or mermaid` | unknown `format` |
| `401` | `unauthorized` | missing or wrong bearer token |
| `404` | `action not found` | no such action in the retained state |
| `500` | `graph build failed` | internal error while building |
| `500` | `graph failed validation` | the built graph did not pass `model.Validate` |
| `503` | `not ready` (plain text, `/readyz` only) | informers not synced |

## Consistency

Graphs are built on demand from the in-memory store at request time. An
`OBSERVING` or `SETTLING` graph can change between requests; a `COMPLETE`
graph changes only if late observations for its windows arrive. Once the
underlying observations age out of the retention period (2 h by default),
the action disappears and its ID returns `404`. Use `effecttrace export` or a
collector recording to keep graphs.

## Timeouts

The API server uses a 5 s read-header timeout, a 10 s read timeout and a 30 s
write timeout.
