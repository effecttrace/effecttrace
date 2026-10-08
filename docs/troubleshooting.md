# Troubleshooting

Start with `effecttrace doctor`. It checks `/healthz` and `/readyz`, prints
the health and detail of every source, and shows how many records are
retained. Then open the `COVERAGE` section of the graph in question: a missing
source is reported there instead of being guessed around.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/art/degraded-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/art/degraded-light.svg">
  <img alt="Coverage section showing which sources were available for a graph" src="assets/art/degraded-light.svg" width="100%">
</picture>

```sh
effecttrace doctor --server http://127.0.0.1:18080
effecttrace explain --action <id> --verbose
```

## The action does not appear

| Symptom | Likely cause | Check |
| --- | --- | --- |
| A tool call is missing | the MCP server does not emit a SERVER span with `mcp.method.name = tools/call`, or spans do not reach EffectTrace | `effecttrace_otlp_spans_total{result="accepted"}`; the OpenTelemetry Collector pipeline exports to `http://<effecttrace>:4318`; the span kind is SERVER |
| Spans are rejected | invalid IDs, implausible times, control characters, oversized attributes | `effecttrace_otlp_spans_total{result="rejected"}`, `effecttrace_ingest_records_total{kind="span",result="rejected"}`; run with `--log-level=debug` |
| Spans are throttled | the ingest queue is full; the receiver answers 503 | `effecttrace_queue_depth`, `effecttrace_otlp_spans_total{result="throttled"}`; increase `--queue-size` and keep exporter retries on |
| A `kubectl` change is missing | no audit log, the audit policy drops it, the user matches a controller pattern, the resource is ignored, the request was a dry run or failed | `doctor` shows `kubernetes-audit`; check the policy; see [Kubernetes correlation](kubernetes-correlation.md#controller-identities-and-actors) |
| Actions disappeared | older than the retention period (2 h by default) or evicted by store bounds | `--retention`; `effecttrace_ingest_records_total{result="applied"}` |

## The graph is degraded

| Symptom | Meaning | What to do |
| --- | --- | --- |
| Tool call reaches its request only via `TEMPORAL CORRELATION`; nodes are `CORRELATED` | no client span descends from the tool span | wrap the Kubernetes client with `k8sinstrument.Wrap` and pass the tool span's context to client calls ([MCP](mcp.md#instrumenting-your-own-mcp-server)) |
| Note "No Kubernetes request is linked to this tool call by trace context or audit ID." | same as above, and no request fell inside the tool span either | as above |
| `actor` missing on a tool call | the agent did not propagate `traceparent` in `params._meta`, so the tool span is a root span | propagate context in the MCP client |
| Coverage `kubernetes-audit no` | requests are not confirmed by audit events | check `--audit-log`, file permissions, the audit policy, and that the collector runs on the node that writes the log |
| Note about an audit ID that disagrees | a client span named an audit ID whose event does not match its verb or object | possible forged or misattributed span; see the [threat model](threat-model.md) |
| Target shown as `k8s:unresolved:...` | no UID from the response or audit, and name resolution found zero or several objects | ensure the watch covers the namespace (`--namespaces`) and was running before the action |
| Coverage `kubernetes-watch no`, "watch started after the action" | the collector (re)started after the action | earlier changes are unknown; replay a recording if you have one |
| Object `UNCHANGED`, no fan-out | the request succeeded but no generation increase, creation or deletion was observed near it | expected for no-op requests ([ADR-008](adr/008-no-op-requests-and-change-level-claims.md)) |
| Changes under `NOT ATTRIBUTED (ambiguous)` | another action's window also covers them | expected for concurrent actions on the same workload |
| Expected changes under `EXCLUDED` | another action claims them, or no ownership path exists | read the reason and `claimedBy`; only controller ownership is followed |

## The graph stays OBSERVING or SETTLING

| Status | Cause | What to do |
| --- | --- | --- |
| `OBSERVING` for minutes | a workload has not reported a stable status; the window ends at `--max-reconcile` (3 min by default) | check the rollout (`kubectl rollout status`); a failed rollout is capped by design |
| `SETTLING` | telemetry evaluation pending or failing | `doctor` shows `prometheus`; check `--prometheus-url`, network access and `effecttrace_telemetry_evaluations_total{result="error"}` |
| Prometheus coverage "all signal queries failed" | query errors, more than one series per query, or a non-DNS-1123 workload name | run the signal's query in Prometheus with `$namespace` and `$workload` substituted; aggregate to one series |

## Collector startup errors

| Message | Fix |
| --- | --- |
| `--kube=kubeconfig requires an explicit --kube-context` | pass `--kube-context`; the current context is never used implicitly |
| `--prometheus-url requires --signals` | add a signals file |
| `invalid signal name`, `query must reference $workload`, `thresholds must be non-negative`, `reduce must be max or mean` | fix the signal file; unknown keys are also rejected |
| `invalid --identity-mode` | use `pseudonymize` or `keep` |
| `/readyz` stays `not ready` | informers cannot sync: check RBAC (get, list, watch on the observed kinds) and API connectivity |

## Lab problems

| Symptom | Fix |
| --- | --- |
| `no container engine found` | install Docker or Podman, or set `CONTAINER_ENGINE` |
| `refusing: context kind-effecttrace-lab does not point at a local kind API server` | `.lab/kubeconfig` is stale or edited; run `make demo-down` then `make demo-up` |
| `cluster effecttrace-lab does not exist` | run `make demo-up` |
| Rollouts time out during `make demo-up` | the container VM lacks CPU or memory; give Podman or Docker more resources |
| Experiments stop with "lab clock differs from the host" | the container VM's clock drifted (common after a laptop sleeps); resynchronize it, for example by restarting the Podman machine, then rerun |
| The demo waits for "status COMPLETE" and times out | `effecttrace doctor --server http://127.0.0.1:18080`; check that the OpenTelemetry Collector is running in `observability` |
| Images are stale after code changes | `make demo-up` on an existing lab rebuilds and reloads the images and restarts the collector and the tool server; `make images` only rebuilds and reloads, so restart with `./scripts/lab.sh kubectl -n effecttrace-system rollout restart deployment/effecttrace-collector` |

## Getting more detail

- `--log-level=debug` logs rejected records and skipped audit lines.
- `effecttrace explain --verbose` prints every node and every edge reason.
- `effecttrace graph --format json` shows facts and rules for every edge.
- A recording (`--record`) plus `effecttrace replay` reproduces a graph
  offline, which is the best input for a bug report.

When filing an issue, include the `doctor` output and, if possible, a
minimal recording. Do not include unredacted identities or Secrets; see
[privacy](privacy.md).
