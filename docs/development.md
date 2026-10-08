# Development

This guide covers building, testing and changing EffectTrace. Contribution
rules (sign-off, review, scope) are in [CONTRIBUTING.md](../CONTRIBUTING.md).

## Prerequisites

- Go, at the version in `go.mod`
- `make` and `bash`
- For the lab: Docker or Podman, and `kubectl` (kind is installed into `.bin/`
  automatically)
- `python3` only for `make site-check`

## Make targets

`make` (or `make help`) lists every target. CI runs the same targets.

| Target | What it does |
| --- | --- |
| `make build` | build `bin/effecttrace` and `bin/effecttrace-collector` (`VERSION=` sets the version) |
| `make fmt` | fail if any Go file needs `gofmt` |
| `make vet` | `go vet ./...` |
| `make lint` | `golangci-lint` v2.14.0 (installed into `.bin/`) |
| `make vuln` | `govulncheck` v1.8.0 |
| `make test` | unit and integration tests |
| `make test-unit` | `go test -race -count=1 ./pkg/... ./internal/...` |
| `make test-integration` | in-process integration tests |
| `make fuzz` | every fuzz target for `FUZZTIME` (default 15 s) |
| `make benchmark` | synthetic benchmarks into `test-results/benchmarks.json` |
| `make scan` | scan tracked files and commit metadata for personal data, credentials, private paths and attribution strings |
| `make verify` | `fmt vet lint test scan`: everything CI checks without a cluster |
| `make demo-up` / `demo-run` / `demo-down` | local kind lab ([deployment](deployment.md#local-lab), [demo](demo.md)) |
| `make images` | rebuild and load images into the running lab |
| `make test-e2e` | experiment suite against the lab |
| `make test-results` | regenerate `summary.json` and render `docs/results.md`, `docs/benchmarks.md` and the README results block |
| `make site-check` | validate the project website in `SITE_DIR` (default `../effecttrace.github.io`) |

Before opening a pull request, run `make verify`. For changes to correlation,
sources or the lab, also run the experiments.

## Repository layout

```text
cmd/                    collector and CLI entry points
pkg/model               effect graph types, validation, canonical JSON (public)
pkg/semconv             attribute keys (public)
pkg/k8sinstrument       client-go transport instrumentation (public)
internal/               collector internals (see docs/architecture.md)
demo/                   scripted agent, demo MCP tool server, shop simulator (not EffectTrace)
deploy/                 kind config, audit policy, manifests, Containerfiles
scripts/                lab, demo, hygiene scan, site check
schemas/                JSON Schema of the effect graph
test/integration        in-process integration tests
test/experiments        kind experiment harness, ground truth, evaluation
test/benchmarks         synthetic benchmark runner
docs/                   documentation and ADRs
```

Packages under `pkg/` are importable by other projects. Their APIs are not yet
stable (v0.x); `pkg/model`'s JSON schema version is `effecttrace.io/v1alpha1`.

## Running the collector locally

Against the lab cluster (explicit context, never the current one):

```sh
make build
./bin/effecttrace-collector --kube=kubeconfig --kubeconfig .lab/kubeconfig \
  --kube-context kind-effecttrace-lab --api-listen 127.0.0.1:8081 --otlp-listen 127.0.0.1:4319
./bin/effecttrace doctor --server http://127.0.0.1:8081
```

Without a cluster, replay a recording:

```sh
./bin/effecttrace replay --recording .lab/shared/recording.jsonl --verbose
```

## Changing attribution rules

Attribution is the core of the project. A change must keep these invariants:

1. **Evidence is derived from the relationship.** Create edges only with
   `model.NewEdge` (through `builder.addEdge`). Never set `Evidence` directly.
2. **Every edge names its rule.** Add a constant in
   [`internal/correlate/build.go`](../internal/correlate/build.go) and
   document it in the [evidence model](evidence-model.md#attribution-rules).
3. **Single claimant.** Controller-driven changes are attached only when no
   other action claims them; otherwise record an ambiguity or exclusion.
4. **Determinism.** Graph building is a pure function of the store and
   configuration. Do not read the wall clock except through `now`, and sort
   anything that comes from a map.
5. **Honest degradation.** A missing source lowers coverage or the grade; it
   never produces a stronger claim.

Add a unit test in `internal/correlate/correlate_test.go` built with
[`internal/synth`](../internal/synth), and, when behaviour on a real cluster
matters, an experiment ([testing](testing.md#writing-a-new-experiment)).

If the change affects the JSON shape, update `pkg/model`,
[`schemas/effect-graph.schema.json`](../schemas/effect-graph.schema.json),
[effect graph](effect-graph.md) and, if needed, the schema version.

## Adding a source or attribute

- New span attributes must be added to the OTLP allowlist deliberately and
  documented in [semantic conventions](semantic-conventions.md).
- New observation fields must be bounded in
  [`internal/store/validate.go`](../internal/store/validate.go) and reviewed
  for privacy ([privacy](privacy.md)).
- New watched kinds must be stripped in `kube.Strip` and added to the observer
  RBAC.
- New metrics must use bounded labels only.

## Style

- `gofmt`, `go vet` and the `golangci-lint` configuration in `.golangci.yml`
  (standard linters plus `bodyclose`, `errorlint`, `gosec`, `misspell`,
  `nilerr`, `noctx`, `unconvert`, `usestdlibvars`).
- Errors wrap with `%w`; inputs are validated, not truncated, when they are
  identifiers.
- Comments explain why, especially for evidence decisions.
- Documentation states configuration defaults from code, and never types
  measured numbers; those are generated from `test-results/`.

## Documentation

Docs live in `docs/`. Architecture decisions are recorded as ADRs in
[`docs/adr`](adr/README.md); add one for any decision that changes the
evidence model, storage, privacy defaults or export semantics.
