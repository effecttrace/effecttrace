# Dependencies

This file lists EffectTrace's direct dependencies, the runtime images used by
the local lab, and the development tools. Licenses were checked against the
`LICENSE` files of the exact module versions in the Go module cache. Versions
come from `go.mod`, the lab manifests and the `Makefile`.

## Go modules (direct)

Go version: 1.27.1 (`go.mod`).

| Module | Version | License | Purpose | Used by | Governance |
| --- | --- | --- | --- | --- | --- |
| `k8s.io/client-go` | v0.37.1 | Apache-2.0 | informers, in-cluster and kubeconfig clients | collector (runtime); `pkg/k8sinstrument` users; demo tool server; experiments | Kubernetes, CNCF graduated |
| `k8s.io/api` | v0.37.1 | Apache-2.0 | Kubernetes API types | collector, demo, tests | Kubernetes, CNCF graduated |
| `k8s.io/apimachinery` | v0.37.1 | Apache-2.0 | object metadata, types | collector, demo, tests | Kubernetes, CNCF graduated |
| `sigs.k8s.io/yaml` | v1.6.0 | MIT and BSD-3-Clause (bundled go-yaml forks carry their own MIT and Apache-2.0 notices) | strict parsing of the signals file | collector | Kubernetes SIGs (`kubernetes-sigs`) |
| `go.opentelemetry.io/otel` | v1.47.0 | Apache-2.0 | OpenTelemetry API | collector (graph export), `pkg/k8sinstrument`, demo | OpenTelemetry, CNCF graduated |
| `go.opentelemetry.io/otel/trace` | v1.47.0 | Apache-2.0 | tracing API | collector, `pkg/k8sinstrument`, demo | OpenTelemetry, CNCF graduated |
| `go.opentelemetry.io/otel/sdk` | v1.47.0 | Apache-2.0 | tracer provider for graph export | collector, demo | OpenTelemetry, CNCF graduated |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` | v1.47.0 | Apache-2.0 | OTLP/HTTP span export | collector (graph export), demo | OpenTelemetry, CNCF graduated |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.72.0 | Apache-2.0 | client spans for Kubernetes requests | `pkg/k8sinstrument`, demo tool server (not linked into the collector) | OpenTelemetry, CNCF graduated |
| `go.opentelemetry.io/proto/otlp` | v1.11.1 | Apache-2.0 | OTLP protobuf messages for the receiver | collector, tests | OpenTelemetry, CNCF graduated |
| `google.golang.org/protobuf` | v1.36.12 | BSD-3-Clause | OTLP decoding; `protowire` for Kubernetes protobuf metadata | collector, `pkg/k8sinstrument` | Go protobuf project (Google) |
| `github.com/prometheus/client_golang` | v1.24.1 | Apache-2.0 | the collector's `/metrics`; the demo shop's metrics | collector, demo shop simulator | Prometheus, CNCF graduated |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | Apache-2.0 for new contributions; contributions not yet relicensed remain MIT (the module's `LICENSE` documents the transition) | MCP client and server for the demo agent and demo tool server | demo and experiments only (not linked into the collector or the CLI) | Model Context Protocol project, part of the Agentic AI Foundation, a Linux Foundation directed fund |

The `effecttrace` CLI uses only the Go standard library and EffectTrace's own
packages.

### Transitive modules and SBOM

The full module graph, including indirect dependencies, is listed by:

```sh
go list -m all
```

To produce an inventory with licenses or an SBOM, run your preferred tool
against the module graph (for example `go version -m bin/effecttrace-collector`
for the modules linked into a binary). Published releases will attach SBOMs
once release automation exists ([roadmap](ROADMAP.md)). Dependabot proposes
weekly updates for Go modules (grouped for Kubernetes and OpenTelemetry) and
GitHub Actions.

## Lab runtime images

| Image | Version | License | Purpose | Governance |
| --- | --- | --- | --- | --- |
| `kindest/node` | v1.37.0 (default node image of kind v0.33.0, pinned by digest) | Apache-2.0 | single-node Kubernetes cluster | Kubernetes (kind is a SIG Testing subproject) |
| `docker.io/otel/opentelemetry-collector` | 0.162.0, core distribution, pinned by digest | Apache-2.0 | receives application spans and forwards them to EffectTrace; receives exported graphs (`debug` exporter) | OpenTelemetry, CNCF graduated |
| `docker.io/prom/prometheus` | v3.15.0, pinned by digest | Apache-2.0 | scrapes the demo shop; answers signal queries | Prometheus, CNCF graduated |
| `localhost/effecttrace/collector` | built locally (`FROM scratch`) | Apache-2.0 | EffectTrace collector and CLI | this project |
| `localhost/effecttrace/demo` | built locally (`FROM scratch`) | Apache-2.0 | demo shop simulator and demo MCP tool server | this project |

The OpenTelemetry Collector and Prometheus are used unmodified.

## Development tools

| Tool | Version | License | Purpose | Installed by |
| --- | --- | --- | --- | --- |
| Go toolchain | per `go.mod` | BSD-3-Clause | build and test | user |
| kind | v0.33.0 | Apache-2.0 | local lab cluster | `scripts/lab.sh` into `.bin/` |
| golangci-lint | v2.14.0 | GPL-3.0 (tool only; not linked into EffectTrace) | linting | `make lint` into `.bin/` |
| govulncheck (`golang.org/x/vuln`) | v1.8.0 | BSD-3-Clause | known-vulnerability scan | `make vuln` via `go run` |
| kubectl | any recent | Apache-2.0 | lab scripts | user |
| Docker Engine or Podman | any recent | Apache-2.0 (both engines; desktop distributions have their own terms) | container engine for kind | user |

## CI services

GitHub Actions workflows use `actions/checkout`, `actions/setup-go`,
`actions/upload-artifact`, `github/codeql-action` and
`ossf/scorecard-action`, each pinned by commit SHA in `.github/workflows/`.

## Adding a dependency

Prefer the standard library. A new direct dependency needs a reason in the
pull request, a compatible license (Apache-2.0, MIT, BSD), an active upstream,
and an entry in this file.
