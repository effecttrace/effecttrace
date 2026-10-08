# Testing

EffectTrace is tested in five layers, from pure functions to a real
Kubernetes cluster. Results of the cluster experiments and the synthetic
benchmarks are generated from `test-results/*.json` and published in
[results](results.md) and [benchmarks](benchmarks.md). This page describes the
method; it intentionally contains no measured numbers.

## Layers

| Layer | What it covers | Needs a cluster | Command |
| --- | --- | --- | --- |
| Unit | model invariants, attribution rules, sources, store, privacy, rendering, API, export | no | `make test-unit` |
| Fuzz | parsers and decoders of untrusted input | no | `make fuzz` |
| Integration | real components wired in one process | no | `make test-integration` |
| Synthetic benchmarks | correlation engine on generated observation streams | no | `make benchmark` |
| Experiments | the whole system on a kind cluster, scored against independent ground truth | yes (kind) | `make demo-up && make test-e2e` |

### Unit tests

`go test -race -count=1 ./pkg/... ./internal/...`. The correlation tests in
[`internal/correlate/correlate_test.go`](../internal/correlate/correlate_test.go)
build observation streams with [`internal/synth`](../internal/synth) and check
named properties, for example:

- a restart attributes the rollout's descendants;
- an unrelated concurrent change is excluded;
- concurrent actions on the same workload produce ambiguities;
- a missing trace link degrades to correlation;
- a missing audit log still yields `DIRECT` evidence from the response;
- a name reused with a new UID does not create false continuity;
- deleting a Pod attributes the replacement;
- the graph is deterministic under reordering;
- a human scale attributes only the new Pods;
- controller requests are not actions;
- a no-op request never claims later effects.

### Fuzz targets

`make fuzz` runs each target for `FUZZTIME` (default 15 s; CI uses 10 s):

| Package | Target | Input |
| --- | --- | --- |
| `./pkg/model` | `FuzzUnmarshalGraph` | graph JSON |
| `./internal/store` | `FuzzApplyRecordJSON` | observation records |
| `./internal/source/otlp` | `FuzzDecodeJSON` | OTLP/JSON |
| `./internal/source/otlp` | `FuzzDecodeProto` | OTLP protobuf |
| `./internal/source/audit` | `FuzzParse` | audit event lines |
| `./internal/privacy` | `FuzzText` | free text sanitization |
| `./pkg/k8sinstrument` | `FuzzParseProtobufMetadata` | Kubernetes protobuf responses |
| `./pkg/k8sinstrument` | `FuzzParseRequest` | HTTP method and API path |

```sh
make fuzz FUZZTIME=60s
# or one target
go test ./internal/source/otlp -run '^$' -fuzz '^FuzzDecodeProto$' -fuzztime 2m
```

### Integration tests

[`test/integration`](../test/integration) wires the real audit tailer (on a
temporary file), the OTLP/HTTP receiver, the Kubernetes watcher on a fake
clientset, the ingest pipeline, the correlation engine and the HTTP API in one
process. No cluster is required.

### Synthetic benchmarks

[`internal/bench`](../internal/bench) and [`test/benchmarks`](../test/benchmarks)
feed the correlation engine generated observation streams (one Deployment with
three Pods per concurrent agent restart) at several concurrency levels and
measure ingest, index-and-graph and graph query time and retained heap per
action. No network, cluster or collector I/O is involved, so these numbers say
nothing about end-to-end or production-scale behaviour. `make benchmark`
writes `test-results/benchmarks.json`; see [benchmarks](benchmarks.md).

## Experiments on kind

The experiment suite ([`test/experiments`](../test/experiments)) runs against
the local lab (see [deployment](deployment.md#local-lab) and [demo](demo.md)).
Each scenario performs scripted actions, waits until the affected workloads
are stable plus a settle period, waits for EffectTrace's graphs to reach
`COMPLETE`, and scores every graph against ground truth.

### Ground truth

Ground truth is computed by the harness **without using EffectTrace output**:

1. **Harness intent.** The harness knows which actions it performed, on which
   Deployment, and when each request started and returned.
2. **An independent watch.** A deliberately simple watch, separate from
   EffectTrace's code ([`truth.go`](../test/experiments/truth.go)), records the
   lifetime of Deployments, ReplicaSets, Pods, Services and ConfigMaps in the
   `shop` namespace on the harness clock: first seen, deleted, controller owner
   UID, and ReplicaSet replica changes.

From these, for each action:

- **Direct** is the UID of the object the action mutated.
- **Structural** is every object transitively controlled by that object that
  was created, deleted or (for a ReplicaSet) rescaled between the action's
  start and the end of the scenario's settle period. Deployments, Services and
  ConfigMaps are not structural targets. For Pod deletions, the replacement
  Pods created by the owner are the structural truth.
- **Unrelated** is every other object that changed in the namespace during the
  scenario, plus every object expected for another action.
- **Workloads** are the workloads whose telemetry is in scope.

**Uncertain in-flight interval.** When a second request on the same workload
is in flight, the harness cannot know on which side of the API server's
receive time a change fell. Changes between the second request's start and its
return are marked **uncertain** and are not scored for either action.

**Change-level ambiguity.** Changes after a competing request on the same
workload are inherently ambiguous: both actions' scopes cover them. The truth
marks them **ambiguous**, and attributing them to either action is counted as
an error (`ambiguousClaimed`), not as a success. The expected behaviour is that
EffectTrace reports them as ambiguities and claims them for neither action
(scenarios L16, L17, L32).

### Metrics

A **claim** is an object node in a graph whose path grade is `ATTRIBUTED`.
Objects reachable only through `TEMPORAL_CORRELATION` (grade `CORRELATED`)
are counted separately and are never claims.

| Metric | Definition |
| --- | --- |
| Direct TP / FP / FN | claims with an incoming `DIRECT` edge compared with the direct truth |
| Structural TP / FP / FN | claims with an incoming `STRUCTURAL` edge, excluding ownership-context edges (`controller-owner-of-directly-mutated-object`), compared with the structural truth; uncertain objects that are not expected are skipped |
| Precision | TP / (TP + FP), undefined when there are no claims |
| Recall | TP / (TP + FN), undefined when nothing was expected |
| False attachments | claims on objects known to be unrelated to the action |
| Ambiguous claims | claims on changes the truth marks as ambiguous (and not uncertain) |
| Unrelated telemetry | `TEMPORAL_CORRELATION` edges to signals of workloads outside the action's telemetry scope |
| Concurrent separation | for concurrent scenarios, whether any action claimed an object expected only for another action |
| First structural effect | seconds from the action to the first structural effect visible through the API |
| Graph complete | seconds from the action until the graph reported `COMPLETE`; includes the configured settle and telemetry windows |
| Graph query | API latency of graph requests while polling |

Every scored action also runs standard checks: no false attachments, no
attributed claim on an ambiguous change, direct and structural precision and
recall, and no unrelated telemetry. Scenarios add their own checks (for
example coverage reported as missing, or graphs unchanged after a late metric
change).

A scenario's status is `pass`, `fail`, `error` (the harness could not
complete it) or `unsupported`. The summary aggregates attribution metrics over
live scenarios only; the replay variants R05 and R06 change the window
configuration on purpose and are excluded. Operational timings are measured on
a single-node kind cluster on a laptop by polling the API once per second, and
are not a production-scale measurement.

### Scenarios

The table is copied from
[`scenarios_live.go`](../test/experiments/scenarios_live.go) and
[`scenarios_replay.go`](../test/experiments/scenarios_replay.go). Run
`go run ./test/experiments/cmd/run --list` for the current list.

| ID | Category | Title |
| --- | --- | --- |
| L01 | live | MCP tool restarts a Deployment |
| L02 | live | MCP tool scales a Deployment up |
| L03 | live | MCP tool scales a Deployment down |
| L04 | live | MCP tool changes an image |
| L05 | live | MCP tool rolls an image back |
| L06 | live | MCP tool triggers a failed rollout |
| L07 | live | MCP tool deletes a Pod |
| L08 | live | MCP tool modifies a ConfigMap |
| L09 | live | MCP tool changes a Service selector |
| L10 | live | MCP tool changes resource limits |
| L11 | live | Human kubectl scale |
| L12 | live | Human kubectl rollout restart |
| L13 | live | Human kubectl delete pod |
| L14 | live | Two agents change two workloads concurrently |
| L15 | live | Agent and human change two workloads concurrently |
| L16 | live | Agent and human change the same workload |
| L17 | live | Two rapid restarts fan out over three ReplicaSets |
| L18 | live | Unrelated latency spike in another service |
| L19 | live | Unrelated error spike in another service |
| L20 | live | Unrelated fault inside the window of the same workload |
| L21 | live | Metric change after the window |
| L22 | live | Same name deleted and recreated with a new UID |
| L23 | live | Trace context missing between agent and tool server |
| L24 | live | Trace context missing between tool and Kubernetes client |
| L25 | live | EffectTrace collector restarts during a rollout |
| L26 | live | OpenTelemetry Collector restarts |
| L27 | live | Audit log unavailable |
| L28 | live | Metrics source unavailable |
| L29 | live | kube-controller-manager restarts during a rollout |
| L30 | live | Hostile and forged telemetry |
| L31 | live | Burst of four concurrent tool calls |
| L32 | live | Burst of eight concurrent tool calls, two per workload |
| L33 | live | Scale to zero, then back up |
| R01 | replay | Replay reproduces live graphs |
| R02 | replay | Out-of-order and duplicated input |
| R03 | replay | Delayed audit stream |
| R04 | replay | Delayed Kubernetes Events |
| R05 | replay | Observation window too short |
| R06 | replay | Observation window very long |
| R07 | replay | Corrupt recording input |
| U01 | unsupported | HorizontalPodAutoscaler changes replicas after load |
| U02 | unsupported | GitOps-driven change |

Replay scenarios rebuild graphs offline from the collector's recording of the
same run (shared with the host through `.lab/shared/`). Unsupported scenarios
are recorded as such and are not executed; they document coverage gaps (see
[limitations](limitations.md)). Scenarios L18 to L21 inject runtime faults
(latency or errors without any Kubernetes change) through the shop's fault
relay and request a longer quiet period first, so that the telemetry baseline
is not disturbed by the previous scenario.

### Running the experiments

```sh
make demo-up                     # create the effecttrace-lab kind cluster
make test-e2e                    # all scenarios -> test-results/
go run ./test/experiments/cmd/run --only L01,L07 --out test-results
make test-results                # summary.json, docs/results.md, docs/benchmarks.md
make demo-down
```

`make test-e2e` writes `test-results/results.json`, `summary.json`,
`environment.json` (component versions and host) and one canonical graph per
scored action under `test-results/graphs/`. Before the first scenario and
after every live scenario, the harness restores the `shop` namespace to its
declared state. Each scenario has a 12-minute timeout; a scenario that ends
with an error or without any evaluated check is reported as `error`. Before
each live scenario the harness compares the lab's clock with the host's and
stops if they differ by more than 1.5 s, because the ground truth is recorded
on the host clock. The harness refuses to run against any cluster other than
the local `kind-effecttrace-lab` context.

`make test-results` regenerates `summary.json` from `results.json` only, then
renders [results](results.md), [benchmarks](benchmarks.md) and the README
results block from the JSON files. Numbers are never typed by hand.

## Continuous integration

| Workflow | Runs |
| --- | --- |
| `ci` (push to main, pull requests) | `make fmt vet lint build test-unit test-integration scan`; fuzzing with `FUZZTIME=10s`; `make vuln`; `make benchmark` (uploaded as an artifact) |
| `e2e` (push to main, weekly, manual) | creates the kind lab on the runner and runs a smoke subset (L01, L02, L07, L08, L11, L14, L16, L24) on push, or the full suite (or chosen IDs) on schedule and dispatch; fails if any scenario fails or errors; uploads `test-results/` |
| `codeql` (push, pull requests, weekly) | CodeQL analysis of Go |
| `scorecard` (push, weekly) | OpenSSF Scorecard |

## Writing a new experiment

1. Add a `Scenario` to `LiveScenarios()` or `ReplayScenarios()` with a new ID,
   a title, a category and a description that states the expected behaviour.
2. Derive truth only from the harness's own actions and the independent
   recorder (`DescendantTruth`, `ReplacementTruth`, `ChangedInNamespace`),
   never from EffectTrace output.
3. Use `standardChecks` and add scenario-specific checks with clear names.
4. Update the table above.

## A note on `firstEffectSeconds`

`test-results/results.json` files generated before this note was added contain a
per-action `firstEffectSeconds` field. It does not measure how quickly a
structural effect became visible: the harness starts polling a graph only after
the workload has stabilized, so the value is the time until the first poll. It
is not used in `summary.json`, `docs/results.md`, the README or the website, and
newer runs no longer emit it. `completeSeconds` is accurate to within the
one-second poll interval and the collector's three-second evaluation tick,
because a graph completes only after polling has started.
