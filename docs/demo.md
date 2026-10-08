# Demo

The demo runs live against the local kind lab. A scripted MCP client (no
language model) asks the demo MCP tool server to change workloads, and
EffectTrace explains the evidence-graded effect graph of each action. Every
step is scripted and reproducible.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/art/terminal-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/art/terminal-light.svg">
  <img alt="effecttrace explain output in a terminal" src="assets/art/terminal-light.svg" width="100%">
</picture>

## Run it

```sh
make demo-up     # once: create the effecttrace-lab cluster and deploy everything
make demo-run    # the scripted demo
make demo-down   # delete the lab cluster
```

`make demo-run` executes [`scripts/demo.sh`](../scripts/demo.sh). It builds
`bin/agent` and `bin/effecttrace`, sends agent spans to the lab's
OpenTelemetry Collector (`http://127.0.0.1:14318`), and queries EffectTrace at
`http://127.0.0.1:18080`.

## What happens

0. **Health check.** `effecttrace doctor` prints API health, readiness and
   which sources are reporting.
1. **One action.** The agent calls `restart_workload(shop/checkout)`. The
   script waits until the graph is `COMPLETE`.
2. **Explanation.** `effecttrace explain --trace-id <trace>` prints the graph:
   the tool call, the PATCH request linked by trace context, the Deployment
   changed with `DIRECT` evidence confirmed by the audit event, the new
   ReplicaSet and Pods and the terminated old Pods as `STRUCTURAL` evidence,
   the controller Events as `EVENT_REFERENCE`, and, only if a configured
   signal changed during the window, a `TEMPORAL_CORRELATION` edge.
3. **An unrelated change at the same moment.** The agent restarts checkout
   again while a human scales `shop/payments` to 3 replicas with `kubectl`.
   The payments changes appear under `EXCLUDED`, not in the agent's graph.
4. **Two agents at once.** One agent scales `shop/inventory` to 3 replicas
   while another restarts `shop/frontend`. Each graph contains only its own
   workload's effects.
5. **Restore.** The script re-applies `deploy/lab/shop.yaml`.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/art/concurrent-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/art/concurrent-light.svg">
  <img alt="Two concurrent actions, each with its own graph" src="assets/art/concurrent-light.svg" width="100%">
</picture>

## Reading the explanation

`effecttrace explain` prints these sections:

| Section | Content |
| --- | --- |
| `ACTION` | name, kind, start time, actor, trace ID, status and action ID |
| effect tree | each node under the node it was reached from, with its evidence tag (`DIRECT`, `TRACE LINK`, `STRUCTURAL`, `EVENT REFERENCE`, `TEMPORAL CORRELATION`) and offsets from the action start |
| `NOT ATTRIBUTED (ambiguous)` | changes claimed by more than one action, with the candidates |
| `EXCLUDED (observed nearby, deliberately not attached)` | nearby changes and signals with the reason and claiming action |
| `WINDOWS` | each window with start and end relative to the action and why it closed |
| `COVERAGE` | each source with yes/no and detail |
| `IMPORTANT` | graph notes, for example "Temporal correlation does not prove causation." |

Use `--verbose` to show every node and the reason for every edge, and
`--color never` for plain output. The same view is served by the API at
`/api/v1/effects/{id}/graph`.

## Try it yourself

```sh
go build -o bin/agent ./demo/cmd/agent
go build -o bin/effecttrace ./cmd/effecttrace
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:14318
export EFFECTTRACE_SERVER=http://127.0.0.1:18080

./bin/agent update_image namespace=shop deployment=payments container=app image=localhost/effecttrace/demo:v2
./bin/effecttrace actions
./bin/effecttrace explain --trace-id <traceId printed by the agent>
./bin/effecttrace graph --trace-id <traceId> --format mermaid
./bin/effecttrace observe          # follow new actions as their graphs complete
```

Other tools: `scale_workload` (`replicas=N`), `restart_workload`,
`delete_pod` (`pod=NAME`), `set_resources` (`container=app cpu_limit=200m
memory_limit=128Mi`), `set_config` (`configmap=shop-config key=K value=V`) and
`update_service_selector` (`service=NAME key=K value=V`). Human actions work
too:

```sh
./scripts/lab.sh kubectl -n shop rollout restart deployment/frontend
./bin/effecttrace actions --since 5m
```

To see degraded evidence, call the agent with `--no-trace-context`, or restart
the tool server with `UNINSTRUMENTED_KUBERNETES_CLIENT=true`, and compare the
graphs. Re-apply `deploy/lab/shop.yaml` to restore the namespace.

## Offline replay

The lab collector records every applied observation to `.lab/shared/recording.jsonl`:

```sh
./bin/effecttrace replay --recording .lab/shared/recording.jsonl --action <id>
./bin/effecttrace replay --recording .lab/shared/recording.jsonl --out /tmp/graphs
```

## What the demo is not

The demo agent and tool server are reference actors for the lab. They are not
part of EffectTrace, they are not an agent framework, and the shop is a
fictional workload. The demo shows how evidence classes behave; it is not a
production measurement. Measured results are in [results](results.md).
