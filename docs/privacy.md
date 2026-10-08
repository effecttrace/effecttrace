# Privacy

EffectTrace observes infrastructure activity, which can include who did what
and the shape of their workloads. Its defaults keep the minimum it needs to
record evidence-graded relationships and nothing more
([ADR-006](adr/006-privacy-defaults.md)).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/art/privacy-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/art/privacy-light.svg">
  <img alt="Data minimization: what each source keeps and what it drops" src="assets/art/privacy-light.svg" width="100%">
</picture>

## Summary of defaults

| Data | Default handling | Where |
| --- | --- | --- |
| Pod specs, environment variables, container commands, images, volumes | dropped before caching | informer `Strip` transform |
| ConfigMaps, Secrets | never watched; no RBAC access | collector RBAC |
| Request and response bodies | never retained | audit parser, `k8sinstrument` |
| Audit `requestURI`, source IPs | never retained (the URI is inspected only for `dryRun=`) | audit parser |
| Human Kubernetes usernames | pseudonymized with a keyed HMAC | `privacy.Policy` |
| `system:*` identities | kept verbatim | `privacy.Policy` |
| User agents | reduced to the first product token, at most 64 bytes | `privacy.UserAgent` |
| Event messages | control characters removed, whitespace collapsed, truncated to 256 bytes | `privacy.Text` |
| MCP tool arguments and results | always dropped, even when a producer sends them | OTLP attribute allowlist |
| Span attributes | allowlist only | OTLP receiver |

## Kubernetes objects

The informers use a transform (`kube.Strip`) that runs **before** an object
enters the informer cache. It keeps:

| Kind | Kept |
| --- | --- |
| all watched kinds | name, namespace, UID, resourceVersion, generation, creationTimestamp, deletionTimestamp, ownerReferences |
| Deployment | `spec.replicas`, status, the `deployment.kubernetes.io/revision` annotation |
| ReplicaSet | `spec.replicas`, status, the revision annotation, the `pod-template-hash` label |
| StatefulSet | `spec.replicas`, status |
| DaemonSet | status |
| Job | active, succeeded and failed counts |
| Pod | phase, the Ready condition, the `pod-template-hash` label |
| Service | metadata only |
| Event | involved object reference, reason, message, type, count, series, reporting controller or source component, timestamps |

Everything else (all other labels and annotations, the pod template, container
specs, environment, commands, volumes) is discarded. ConfigMaps and Secrets are
not watched at all.

## Audit events

The audit parser reads only `ResponseComplete` events of mutating verbs and
keeps audit ID, verb, API group and version, resource, subresource, namespace,
name, `objectRef.uid`, user (or impersonated user), user agent, response code,
a dry-run flag and the two timestamps. It never keeps request or response
objects, the request URI, or source IPs.

The lab audit policy ([`deploy/kind/audit-policy.yaml`](../deploy/kind/audit-policy.yaml))
uses `Metadata` as its most detailed level, so request and response bodies,
including Secret and ConfigMap contents, are never written to the audit log in
the first place. It also omits the `RequestReceived` stage and does not audit
reads, leases, Events, `system:kube-proxy` or health endpoints. Use a policy
like it in production: EffectTrace needs nothing above `Metadata`.

## Identities

`--identity-mode` controls usernames from the audit log:

- `pseudonymize` (default): usernames starting with `system:` (controllers,
  nodes, service accounts) are kept verbatim; every other username becomes
  `user:` followed by the first 12 hex digits of HMAC-SHA-256 of the username.
- `keep`: usernames are stored verbatim.

The HMAC key is read from the environment variable named by
`--identity-key-env` (default `EFFECTTRACE_IDENTITY_KEY`) and must hold at
least 16 bytes. Keyed pseudonyms cannot be reversed by hashing candidate
usernames. The collector never pseudonymizes with an empty key: when no key is
configured it generates a random per-process key and logs a warning, so
pseudonyms stay protected but change whenever the collector restarts. The lab
Deployment reads the key from the optional Secret `effecttrace-identity` (key
`key`). `make demo-up` does not create that Secret, so lab pseudonyms use a
random per-process key unless you create it:

```sh
kubectl -n effecttrace-system create secret generic effecttrace-identity \
  --from-literal=key="$(openssl rand -hex 32)"
```

Pseudonyms are stable for a given key, so an operator can see that two
actions came from the same person without seeing who it was. Rotating the key
changes all pseudonyms.

## User agents

`kubectl/v1.37.0 (linux/amd64) kubernetes/abc123` becomes `kubectl/v1.37.0`.
Host and platform details are dropped.

## Telemetry

The OTLP receiver keeps only MCP tool-call spans and Kubernetes client spans,
and of those only an allowlist of attributes
([OpenTelemetry](opentelemetry.md#which-spans-are-kept)). `gen_ai.tool.call.arguments`
and `gen_ai.tool.call.result` are never retained. Baggage is not read.

Prometheus results keep the signal name, unit, namespace, workload name and
UID, the baseline and observed values, the sample count and, on failure, a
sanitized error message of at most 200 bytes.

## Retention and recordings

Observations live in memory for the retention period (2 h by default,
`--retention`) and are lost on restart. With `--record`, applied observations
are also appended to a JSON-lines file created with mode `0600`, up to
`--record-max-bytes` (256 MiB by default). A recording contains exactly the
minimized records described above, including pseudonyms, not raw input.
Protect and delete recordings like any operational log. The CLI writes output
files with mode `0600` as well.

## Exported graphs

Graphs exported over OTLP or served by the API contain object names, UIDs,
namespaces, pseudonymized or system identities, reduced user agents, Event
reasons and sanitized notes, and signal values. Treat them with the same care
as the cluster's audit data, and restrict the API
([security model](security-model.md)).

## Related documents

- [Threat model](threat-model.md)
- [Security model](security-model.md)
