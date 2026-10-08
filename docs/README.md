# EffectTrace documentation

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/art/hero-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/art/hero-light.svg">
  <img alt="EffectTrace: from an action to its observed effects, with the evidence for each edge" src="assets/art/hero-light.svg" width="100%">
</picture>

EffectTrace records evidence-graded relationships between an action on a
Kubernetes cluster (an MCP tool call or a Kubernetes API request) and what was
observed after it. It helps operators follow observed action-to-effect
relationships, and says for every edge what supports it: a request identifier,
Kubernetes ownership, an Event reference, or only timing.

It composes OpenTelemetry, the Kubernetes audit log, Kubernetes object
metadata and Events, and Prometheus. It replaces none of them. It is not a
tracing backend, an MCP gateway, an AIOps or root-cause system, and it does
not infer causation. Temporal correlation does not prove causation.

## Start here

| If you want to | Read |
| --- | --- |
| understand what an edge means | [Evidence model](evidence-model.md) |
| see it working | [Demo](demo.md) |
| know what it cannot do | [Limitations](limitations.md) |
| see measured results | [Results](results.md) and [benchmarks](benchmarks.md) (generated) |
| run it | [Deployment](deployment.md) |

## Concepts

- [Evidence model](evidence-model.md): evidence classes, relationships, path
  grades, attribution rules, single-claimant rule, ambiguity, exclusions,
  coverage, statuses.
- [Effect graph](effect-graph.md): nodes, edges, windows, canonical JSON and the
  [JSON Schema](../schemas/effect-graph.schema.json).
- [Kubernetes correlation](kubernetes-correlation.md): Audit-ID and response
  metadata, UID resolution, no-op requests, structural scope, windows,
  controller identities.
- [Architecture](architecture.md): components, data flow, store bounds.

## Integrations

- [OpenTelemetry](opentelemetry.md): OTLP receiver, how spans become evidence,
  graph export.
- [MCP](mcp.md): trace context in `params._meta`, the demo tool server,
  instrumenting your own server.
- [Prometheus](prometheus.md): signal evaluation and EffectTrace's own metrics.
- [Semantic conventions](semantic-conventions.md): every attribute and span
  name.
- [Upstream compatibility](upstream-compatibility.md): tested versions and
  upstream behaviour relied on.

## Operations

- [Deployment](deployment.md): local kind lab and adapting the manifests.
- [CLI reference](cli.md): every flag of `effecttrace-collector` and
  `effecttrace`.
- [HTTP API](api.md): routes, parameters, errors.
- [Troubleshooting](troubleshooting.md)

## Security and privacy

- [Privacy](privacy.md): data minimization defaults.
- [Threat model](threat-model.md)
- [Security model](security-model.md): observer vs actor RBAC, the audit log
  reader tradeoff, network exposure.
- [Security policy](../SECURITY.md)

## Quality

- [Testing](testing.md): test layers, ground-truth method, metrics, scenarios.
- [Results](results.md) and [benchmarks](benchmarks.md): generated from
  `test-results/*.json`.
- [Limitations](limitations.md)

## Project

- [Development](development.md) and [contributing](../CONTRIBUTING.md)
- [Releasing](releasing.md)
- [Architecture decision records](adr/README.md)
- [Governance](../GOVERNANCE.md), [roadmap](../ROADMAP.md),
  [dependencies](../DEPENDENCIES.md), [changelog](../CHANGELOG.md),
  [code of conduct](../CODE_OF_CONDUCT.md)
