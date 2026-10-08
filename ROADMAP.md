# Roadmap

Everything on this page is **planned** work. None of it exists in the current
code. Items are not commitments and have no dates; priorities may change
based on experiment results and user feedback. What exists today is described
in the [documentation](docs/README.md) and summarized in the
[changelog](CHANGELOG.md).

## Planned: packaging and operations

- **Helm chart** for the collector, with the observer RBAC, NetworkPolicy,
  token Secret and identity key Secret wired in.
- **Published release artifacts**: signed binaries and container images with
  checksums and SBOMs ([releasing](docs/releasing.md)).
- **Authentication and authorization for the API**: TLS, user identity and
  per-namespace access to graphs, instead of a single shared bearer token.
- **Durable storage option** so graphs and observations survive restarts and
  can be kept longer than the in-memory retention.
- **Multi-replica collector** for availability, which requires shared or
  partitioned state.

## Planned: evidence and correlation

- **Service and EndpointSlice relationships**, with their own evidence rules,
  so Service selector changes can show their effect on endpoints.
- **CBOR response metadata** in `pkg/k8sinstrument`, in case client-go or the
  API server negotiate CBOR for built-in types.
- **resourceVersion ordering** where Kubernetes guarantees it (KEP-5504), to
  tighten matching between requests and watch notifications.
- **GitOps integration**, so changes applied by a GitOps controller can carry
  commit-level context instead of appearing only as API calls of its service
  account.

## Planned: evaluation

- **StatefulSet and Job lab experiments** (the code handles these kinds, but no
  experiment exercises them yet).
- **HorizontalPodAutoscaler experiment** with metrics-server in the lab
  (scenario U01).
- **GitOps experiment** once an integration exists (scenario U02).

## Candidates under consideration

These are not yet planned and need an issue and, where relevant, an ADR:

- an audit webhook input in addition to the log file backend;
- reading Deployment conditions in addition to replica counters for stable
  status;
- span-link (`TRACE_LINK` relationship) correlation between spans.

## Not planned

To keep the scope honest, these are explicitly **not** goals: automatic
causal inference, root-cause analysis, language-model-based explanations,
acting as an MCP gateway or proxy, blocking or approving actions, and storing
traces or metrics as a backend.
