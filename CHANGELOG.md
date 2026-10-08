# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

No version has been released yet. v0.1.0 is not released. The entries below
summarize what exists on `main`.

### Added

- **Effect graph model** (`pkg/model`): actions, nodes, evidence-graded edges,
  observation windows, ambiguities, exclusions, coverage and notes; schema
  version `effecttrace.io/v1alpha1`; canonical JSON with deterministic
  ordering; strict decoding that rejects unknown fields; JSON Schema in
  `schemas/effect-graph.schema.json`.
- **Evidence classes** `DIRECT`, `TRACE_LINK`, `STRUCTURAL`,
  `EVENT_REFERENCE` and `TEMPORAL_CORRELATION`, with `INFERRED` reserved and
  not emitted; each relationship fixed to one evidence class; path grades
  `ATTRIBUTED`, `CORRELATED` and `INFERRED` computed with the weakest-link rule.
- **Correlation engine** (`internal/correlate`): MCP tool-call and Kubernetes
  API-call action discovery; target UID resolution from client response
  metadata, audit `objectRef`, or name at request time; no-op detection;
  controller ownership fan-out by UID; replacement Pods after Pod deletion;
  reconciliation windows that close on a stable status; single-claimant
  attribution with ambiguities and exclusions; Kubernetes Event references;
  named attribution rules on every edge.
- **Kubernetes client instrumentation** (`pkg/k8sinstrument`): records the
  server-generated Audit-ID and response object kind, UID, resourceVersion and
  generation (JSON and Kubernetes protobuf) on client spans; never sets the
  Audit-ID header.
- **Collector** (`effecttrace-collector`): OTLP/HTTP trace receiver (protobuf
  and JSON, gzip, 8 MiB bound, 503 backpressure, attribute allowlist);
  kube-apiserver audit log tailer with rotation; stripped client-go informers
  for workloads, Pods, Services and Events; bounded idempotent in-memory store;
  recording for offline replay; Prometheus signal evaluation against a
  baseline; OTLP export of completed graphs as a new trace with a span link;
  read-only HTTP API with optional bearer token; own Prometheus metrics with
  bounded labels.
- **CLI** (`effecttrace`): `actions`, `explain`, `graph` (JSON, DOT, Mermaid,
  text), `export`, `observe`, `replay`, `doctor`, `version`.
- **Privacy defaults**: keyed pseudonymization of human usernames, reduced user
  agents, sanitized and truncated Event notes, no ConfigMap or Secret watches,
  tool arguments and results always dropped.
- **Local kind lab** (`make demo-up`) with OpenTelemetry Collector 0.162.0,
  Prometheus v3.15.0 and Kubernetes v1.37.0; lab safety rules that only touch
  the `effecttrace-lab` cluster through a private kubeconfig; Podman support.
- **Demo** (`make demo-run`): scripted MCP agent (no language model), demo MCP
  tool server with scoped RBAC, and a fictional shop workload.
- **Testing**: unit tests, fuzz targets for every parser of untrusted input,
  in-process integration tests, synthetic benchmarks, and a kind experiment
  suite scored against independent ground truth; results generated from
  `test-results/*.json` into `docs/results.md` and `docs/benchmarks.md`.
- **CI**: formatting, vet, lint, race-enabled tests, fuzzing, govulncheck,
  benchmarks, CodeQL, OpenSSF Scorecard and kind end-to-end experiments.
- **Documentation**: architecture, evidence model, Kubernetes correlation,
  OpenTelemetry, MCP, Prometheus, privacy, threat and security models,
  limitations, testing, operations guides and ADRs 001 to 008.

[Unreleased]: https://github.com/effecttrace/effecttrace/commits/main
