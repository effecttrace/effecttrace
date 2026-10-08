# CLI reference

EffectTrace ships two binaries:

- `effecttrace-collector`: ingests observations and serves the API;
- `effecttrace`: queries a collector, explains graphs and replays recordings.

Build both with `make build` (output in `bin/`). Set the version with
`make build VERSION=v0.1.0`.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/art/terminal-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/art/terminal-light.svg">
  <img alt="effecttrace explain output in a terminal" src="assets/art/terminal-light.svg" width="100%">
</picture>

## effecttrace-collector

```text
effecttrace-collector [flags]
```

| Flag | Default | Description |
| --- | --- | --- |
| `--api-listen` | `:8080` | Address for the read-only API, `/metrics` and health endpoints. |
| `--api-token-file` | empty | File containing a bearer token required on `/api/*`. Surrounding whitespace is trimmed. |
| `--otlp-listen` | `:4318` | Address for the OTLP/HTTP trace receiver. Empty disables it. |
| `--audit-log` | empty | Path of the kube-apiserver audit log (JSON lines). Empty disables audit input. |
| `--audit-from-end` | `false` | Start reading the audit log at its end instead of its beginning. |
| `--kube` | `in-cluster` | Kubernetes access: `in-cluster`, `kubeconfig` or `none`. |
| `--kubeconfig` | empty | kubeconfig path when `--kube=kubeconfig` (default loading rules otherwise). |
| `--kube-context` | empty | kubeconfig context. **Required** with `--kube=kubeconfig`; the current context is never used implicitly. |
| `--namespaces` | all | Comma-separated namespaces to watch. |
| `--prometheus-url` | empty | Prometheus base URL for signal evaluation. Empty disables it. Requires `--signals`. |
| `--signals` | empty | YAML file defining telemetry signals ([Prometheus](prometheus.md#signal-file)). |
| `--prometheus-step` | `2s` | `query_range` step. |
| `--record` | empty | Append applied observations to this JSON-lines file for replay (mode `0600`). |
| `--record-max-bytes` | `268435456` (256 MiB) | Maximum recording size; recording stops when reached. |
| `--otlp-export-endpoint` | empty | `host:port` to export completed graphs as OTLP/HTTP spans. |
| `--otlp-export-insecure` | `false` | Use plain HTTP for graph export. |
| `--identity-mode` | `pseudonymize` | `pseudonymize` or `keep` human usernames ([privacy](privacy.md#identities)). |
| `--identity-key-env` | `EFFECTTRACE_IDENTITY_KEY` | Environment variable holding the pseudonymization key. |
| `--queue-size` | `10000` | Ingest queue capacity. |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON on stderr. |
| `--max-reconcile` | `3m` | Maximum reconciliation window. |
| `--settle` | `5s` | Window extension after a stable status. |
| `--baseline` | `1m` | Telemetry baseline before an action. |
| `--retention` | `2h` | How long observations are kept. |
| `--version` | | Print the version and exit. |

Configuration not exposed as flags (code defaults in
[`internal/correlate/config.go`](../internal/correlate/config.go)): skew
tolerance 500 ms, telemetry settle 15 s, controller and actor username
patterns, ignored resources, temporal fallback enabled, at most 50 exclusions
per graph, ownership depth 4.

Exit status: `1` if startup validation fails (for example `--prometheus-url`
without `--signals`, an invalid signal file, `--kube=kubeconfig` without
`--kube-context`, an unknown `--identity-mode`) or a component fails at
runtime (for example the API port is in use); `2` for an invalid flag or
`--log-level`. A missing audit log file is not fatal: the source is reported
unhealthy and retried.

## effecttrace

```text
effecttrace <command> [flags]
```

Common behaviour:

- `--server URL` selects the collector API. Default: `$EFFECTTRACE_SERVER`,
  otherwise `http://127.0.0.1:8080`. Only `http` and `https` URLs are
  accepted.
- The bearer token, if the collector requires one, is read from
  `$EFFECTTRACE_TOKEN`.
- `--color auto|always|never`: `auto` colors only when stdout is a terminal
  and `NO_COLOR` is unset.
- HTTP requests time out after 30 s; responses are limited to 16 MiB and every
  graph is validated with `model.UnmarshalGraph`.

### actions

List observed actions.

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | see above | collector API |
| `--since` | `1h` | show actions newer than this |
| `--json` | `false` | print the API's JSON |

The table shows start time, kind, status, name and edge counts in the order
`DIRECT/TRACE_LINK/STRUCTURAL/EVENT_REFERENCE/TEMPORAL_CORRELATION`, followed
by the action ID.

### explain

Explain the effect graph of an action as text.

```sh
effecttrace explain --trace-id 4bf92f3577b34da6a3ce929d0e0e4736
effecttrace explain --action k8s-0b8a6f1e-... --verbose
effecttrace explain --file graph.json
```

| Flag | Description |
| --- | --- |
| `--action ID` | action ID |
| `--trace-id ID` | trace ID of an MCP tool call; if the trace holds several actions, the command lists them and asks for `--action` |
| `--file PATH` | read a canonical graph JSON file instead of querying a collector |
| `--verbose` | show every node and the reason for every edge |
| `--out PATH` | write to a file (mode `0600`, no color) |
| `--color` | `auto`, `always`, `never` |
| `--server` | collector API |

### graph

Print a graph as `--format json|dot|mermaid|text` (default `json`). Same
selection flags as `explain`.

```sh
effecttrace graph --action <id> --format dot | dot -Tsvg > graph.svg
effecttrace graph --action <id> --format mermaid
```

### export

Write the canonical JSON graph of an action (same as `graph --format json`).

```sh
effecttrace export --action <id> --out graph.json
```

### observe

Follow new actions and explain each one when its graph reaches `COMPLETE`.

| Flag | Default | Description |
| --- | --- | --- |
| `--interval` | `2s` | poll interval |
| `--color` | `auto` | |
| `--server` | see above | |

Stop with Ctrl-C.

### replay

Rebuild effect graphs offline from a collector recording.

| Flag | Default | Description |
| --- | --- | --- |
| `--recording PATH` | required | JSON-lines recording written by `effecttrace-collector --record` |
| `--out DIR` | empty | write one canonical graph JSON per action into this directory instead of printing |
| `--action ID` | empty | only this action |
| `--close-after` | `1h` | evaluate windows as of this long after the last record |
| `--verbose` | `false` | verbose explanation |
| `--color` | `auto` | |

Invalid lines are skipped and counted on stderr. Replay uses the default
correlation configuration and treats telemetry as configured when the
recording contains metric results.

### doctor

Check collector health and source coverage: `/healthz`, `/readyz`,
`/api/v1/status`, each source's health, and the retained record counts. Exits
non-zero when a check fails.

### version

Print the version (`effecttrace version`, `--version` or `-v`).

## Environment variables

| Variable | Used by | Meaning |
| --- | --- | --- |
| `EFFECTTRACE_SERVER` | `effecttrace` | default collector API URL |
| `EFFECTTRACE_TOKEN` | `effecttrace` | bearer token sent to `/api/*` |
| `EFFECTTRACE_IDENTITY_KEY` | collector (name set by `--identity-key-env`) | pseudonymization HMAC key |
| `NO_COLOR` | `effecttrace` | disables color in `auto` mode |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | demo agent and tool server | where demo spans are sent |
| `CONTAINER_ENGINE` | `scripts/lab.sh` | `docker` or `podman` |
| `KIND` | `scripts/lab.sh` | path of the kind binary (default `.bin/kind`) |
