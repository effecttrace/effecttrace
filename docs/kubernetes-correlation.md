# Kubernetes correlation

This document explains how EffectTrace connects an action to Kubernetes
objects: how `DIRECT` evidence is established, which controller-driven changes
are followed as `STRUCTURAL` evidence, when observation windows close, and
which identities and resources are deliberately ignored. The rules are
implemented in [`internal/correlate`](../internal/correlate) and
[`pkg/k8sinstrument`](../pkg/k8sinstrument).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/art/rollout-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/art/rollout-light.svg">
  <img alt="A Deployment rollout: request, Deployment, new and old ReplicaSets, created and terminated Pods" src="assets/art/rollout-light.svg" width="100%">
</picture>

## Inputs

| Input | What EffectTrace reads | Where |
| --- | --- | --- |
| Instrumented client spans | Kubernetes client spans carrying `effecttrace.k8s.*` attributes | OTLP/HTTP receiver |
| Audit log | `ResponseComplete` events of mutating verbs, reduced to request metadata | JSON-lines log backend file |
| Watch | Deployments, ReplicaSets, StatefulSets, DaemonSets, Jobs, Pods, Services, stripped to identity, ownership and rollout status | client-go informers |
| Events | core/v1 Events, reduced to reference, reason, type, sanitized note, controller and count | client-go informer |

### What is watched, and why ConfigMaps and Secrets are not

The collector watches only the kinds it needs to follow controller ownership
and rollout progress. ConfigMaps and Secrets are never watched: their content
is frequently sensitive, and Kubernetes records no ownership from a ConfigMap
or Secret to the Pods that consume it, so watching them would add exposure
without adding `STRUCTURAL` evidence. A request that changes a ConfigMap is
still recorded as `DIRECT` when the instrumented client reports the UID in the
response (lab experiment L08); it simply has no structural fan-out. The
observer RBAC in [`deploy/collector/collector.yaml`](../deploy/collector/collector.yaml)
does not grant access to ConfigMaps or Secrets.

Objects are stripped by an informer transform *before* they enter the
informer cache. See [privacy](privacy.md) for the exact fields kept.

## Establishing DIRECT evidence

### The instrumented client

[`pkg/k8sinstrument`](../pkg/k8sinstrument) wraps a client-go transport:

```go
cfg.Wrap(k8sinstrument.Wrap)
```

For every mutating request (`create`, `update`, `patch`, `delete`,
`deletecollection`, derived from the HTTP method and URL path), it creates an
OpenTelemetry client span (through `otelhttp`) named `<verb> <resource>` or
`<verb> <resource>/<subresource>`, and records:

- the request target: verb, API group, resource, subresource, namespace and
  name;
- the **server-generated** `Audit-Id` response header, which names the audit
  event of this request;
- from a successful (2xx) response body: `kind`, `metadata.uid`,
  `metadata.resourceVersion` and `metadata.generation`.

Metadata is read from JSON or Kubernetes protobuf (`application/vnd.kubernetes.protobuf`)
responses. client-go's generated clients negotiate protobuf for built-in types,
so the protobuf path decodes only the `runtime.Unknown` envelope, the `TypeMeta`
kind and fields 5 to 7 of `ObjectMeta` without decoding the object. Bodies are
peeked up to 4 MiB (`MaxPeekBytes`) and passed on to the client unchanged;
larger bodies, compressed bodies and other encodings (for example CBOR) pass
through without metadata. Request and response bodies are never recorded.

### Why the Audit-ID header is read and never set

kube-apiserver accepts a client-supplied `Audit-ID` request header without
validation and uses it as the audit event ID (see kubernetes/kubernetes issues
[#127801](https://github.com/kubernetes/kubernetes/issues/127801) and
[#101597](https://github.com/kubernetes/kubernetes/issues/101597)). This
behaviour is undocumented, and it means an audit ID chosen by a client can
collide with or impersonate another request's ID. `k8sinstrument` therefore
**never sets** the header; it only reads the value the server echoes in the
response. Values longer than 128 bytes are ignored.

### Audit confirmation

When a client span names an audit ID and an audit event with that ID exists,
EffectTrace uses the audit event as independent confirmation **only if they
agree** on verb, resource, subresource, namespace and name. On agreement, the
request node becomes `request:<audit ID>`, its time becomes the audit event's
`requestReceivedTimestamp`, and the `DIRECT_REQUEST` edge records
`audit_id_match`. On disagreement, the audit event is not used and the graph
carries a note saying so (lab experiment L30 sends exactly such a forged span).

### Resolving the target UID

The UID of the mutated object is resolved in this order
(`resolveTarget` in [`mutation.go`](../internal/correlate/mutation.go)):

| Order | Source | Rule recorded |
| --- | --- | --- |
| 1 | UID in the response metadata seen by the instrumented client | `uid-from-client-response` |
| 2 | `objectRef.uid` in the audit event | `uid-from-audit-object-ref` |
| 3 | The only object of that kind, namespace and name alive at request time (for `create`: the only one created between the request minus the skew tolerance and the request plus the settle period) | `uid-resolved-by-name-at-request-time` |
| - | No match, or more than one match | `target-not-observed` (no UID asserted) |

**Why `objectRef.uid` is usually empty.** kube-apiserver does not derive
`objectRef.uid` from the URL. It fills it only from the decoded request
object, when one is logged. A PATCH or DELETE request carries no object with
the UID, so the field is empty at every audit level, and a create request has
no UID yet. In practice, audit-only actions such as `kubectl scale`,
`kubectl rollout restart` and `kubectl delete pod` are resolved by name at
request time (lab experiments L11 to L13). That rule depends on the watch: it
refuses to guess when the object was not observed or when several objects
carried the name around the request time (for example a delete-and-recreate,
lab experiment L22).

### Audit-only actions

A successful, non-dry-run, mutating audit event that is not claimed by a tool
call becomes a `KUBERNETES_API_CALL` action, unless its user is a controller
identity or its resource is ignored (see below). Requests with `dryRun=` in
the request URI are ignored. When a request was impersonated, the impersonated
user is the actor.

## No-op requests

A successful request does not always change anything: `kubectl apply` with an
unchanged manifest, a patch that sets a field to its current value, or a
status-only change. EffectTrace attributes controller fan-out only to
mutations whose change was actually observed
([ADR-008](adr/008-no-op-requests-and-change-level-claims.md)):

- **Locating the observation.** With a resourceVersion from the instrumented
  client, the matching watch notification is found by exact equality. Without
  it, EffectTrace takes the first (non-initial) notification between the
  request minus the skew tolerance and **one second after request completion**
  (the audit `stageTimestamp`, or the request time). Later notifications are
  not attributed to an audit-only request, because they may belong to a later
  request. For deletions the bound is extended by the settle period, since
  foreground deletion can take until dependents are gone.
- **Generation-bearing kinds.** For Deployments, ReplicaSets, StatefulSets,
  DaemonSets and Jobs, a `MUTATED` change counts only when
  `metadata.generation` increased. Controller status updates do not change the
  generation and are therefore never mistaken for the action's change.
- **Creation and deletion** count when the corresponding notification is
  observed (the first observation for a create; a DELETED notification or a
  set `deletionTimestamp` for a delete).

A request without an observed change keeps its `DIRECT` edge to the target,
the object's change type becomes `UNCHANGED`, and its window is limited to
`[request - skew, request + settle]` with the reason "no resulting spec
change, creation or deletion was observed". It never claims later effects.

## Structural scope

Starting from each changed object, EffectTrace walks **controller**
`ownerReferences`, matched by **UID**, up to four levels deep (`MaxDepth`).
Non-controller owner references are not followed. Within that scope, three
patterns are attributed:

1. **Descendants of a changed object.** ReplicaSets and Pods (and their
   descendants) that were created, deleted or rescaled inside the mutation's
   window, subject to the single-claimant rule. Unchanged intermediate owners
   are kept as context with the rule
   `controller-owner-of-directly-mutated-object` so the path stays explicit.
2. **Replacement Pods after a Pod deletion.** When an action deletes a Pod
   that has a controller owner, the owner is added as context and Pods that
   the same owner created inside the deletion's window are attributed with
   `replacement-of-deleted-pod-in-window-single-claimant`. Sibling Pods that
   did not change are not touched.
3. **Garbage-collected descendants of a deleted owner.** When an action
   deletes an owner (for example a Deployment), descendants whose deletion is
   observed inside the window are attributed through the same descendant walk.

Creation and deletion of one object are **separate claimable changes**. An
object created by one action may be deleted by another action's rollout; each
change has its own claimants. When the same action both created and deleted an
object inside its window (a Pod of a failed rollout, for instance), the node
shows `CREATED` with `deletedAt`.

Rescaling is detected for ReplicaSets and StatefulSets by a change of desired
replicas between consecutive observations; a rollback that reuses an older
ReplicaSet therefore appears as `SCALED`, not `CREATED` (lab experiment L05).

## Observation windows

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/art/windows-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/art/windows-light.svg">
  <img alt="Reconciliation window from the request until a stable status plus settle, capped at max reconcile" src="assets/art/windows-light.svg" width="100%">
</picture>

Each changed mutation has a reconciliation window:

```text
start = request time - skew tolerance (500 ms)
end   = min(first stable status after the request + settle (5 s),
            request time + max reconcile (3 min))
```

"Stable status" is evaluated from the watch, per kind of the changed object:

| Kind | Stable when (first observation after the request) |
| --- | --- |
| Deployment | `status.observedGeneration` >= the mutation's generation **and** `status.updatedReplicas`, `status.availableReplicas` and `status.replicas` all equal `spec.replicas`; or `spec.replicas` is 0, the generation is observed and `status.replicas` is 0 |
| ReplicaSet | `observedGeneration` >= generation and `readyReplicas` equals `spec.replicas`; or scaled to 0 |
| StatefulSet | as ReplicaSet, and additionally `updatedReplicas` equals `spec.replicas` |
| Pod (non-delete) | at the request time |
| Pod (delete) | when the owner first reports `readyReplicas` equal to `spec.replicas` after the deletion; at the deletion time if the Pod has no observed owner; never if the deletion is not observed (the window is capped) |
| other kinds | when the change was observed, or at the request time |

EffectTrace evaluates these replica counters and `observedGeneration`
directly; it does not read the Deployment `Progressing` or `Available`
conditions. A rollout that never stabilizes (for example an image that does
not exist, lab experiment L06) runs until the cap, and the window reason says
so. While a window is open the graph is `OBSERVING` and the reason reads
"open: waiting for ... to report a stable status".

Windows are configurable: `--max-reconcile`, `--settle` and `--baseline` are
collector flags (see [CLI](cli.md)). The lab collector uses
`--max-reconcile=90s` so failed-rollout experiments finish sooner. The skew
tolerance (500 ms) and the telemetry settle period (15 s) are code defaults
in [`config.go`](../internal/correlate/config.go).

## Controller identities and actors

Requests made by controllers are reconciliation, not actions. By default these
usernames (glob patterns) are treated as controllers:

```text
system:kube-controller-manager
system:kube-scheduler
system:apiserver
system:node:*
system:serviceaccount:kube-system:*
system:serviceaccount:local-path-storage:*
```

Actor patterns override that list for autonomous controllers whose decisions
deserve their own investigation. The default actor list contains the
HorizontalPodAutoscaler controller's service account:

```text
system:serviceaccount:kube-system:horizontal-pod-autoscaler
```

so an HPA scale request is discovered as a `KUBERNETES_API_CALL` action. No lab
experiment exercises the HPA in v0.1 (scenario U01).

## Ignored resources and subresources

Mutations of these resources never become actions:

```text
events, leases, endpoints, endpointslices,
tokenreviews, subjectaccessreviews, selfsubjectaccessreviews,
selfsubjectrulesreviews, selfsubjectreviews, localsubjectaccessreviews,
serviceaccounts/token, certificatesigningrequests,
pods/exec, pods/attach, pods/portforward, pods/proxy,
services/proxy, nodes/proxy, pods/status, pods/binding, pods/eviction,
deployments/status, replicasets/status, statefulsets/status, jobs/status
```

An entry without a slash matches the resource itself, not its subresources.
`deployments/scale` is **not** ignored, so a scale request through the scale
subresource is an action whose target is the parent Deployment.

## resourceVersion

EffectTrace compares resourceVersions **for equality only**, to match the
response of an instrumented request to the corresponding watch notification.
It never orders them. The Kubernetes API conventions now describe
resourceVersions of kube-apiserver built-in types as orderable integers
(KEP-5504, from Kubernetes 1.35), while the `ObjectMeta` documentation still
calls them opaque. Using ordering to tighten observation matching is possible
future work ([roadmap](../ROADMAP.md)).

## Clock skew

Window starts are widened by a skew tolerance of 500 ms. It absorbs the
difference between kube-apiserver timestamps (audit `requestReceivedTimestamp`)
and the collector's own observation times for watch notifications, and the
delay between a change and its watch delivery. When no audit event confirms a
request, the request time is the client span's start time, which comes from
the client's clock. Clocks that differ by more than the tolerance can move a
change in or out of a window; see [limitations](limitations.md).

Object creation times use the collector's observation time for objects created
while watched, and the server `creationTimestamp` for objects delivered by the
initial list.

## Kubernetes API server tracing

kube-apiserver tracing (GA since Kubernetes 1.34) is not used as an input. The
API server uses an incoming `traceparent` as the parent only for privileged
callers and otherwise links it, and audit events carry no trace ID. EffectTrace
instead records the Audit-ID on the client span, which is available for
ordinary callers.

## Related documents

- [Evidence model](evidence-model.md)
- [ADR-002: direct action correlation](adr/002-direct-action-correlation.md)
- [ADR-003: structural relationships](adr/003-structural-relationships.md)
- [ADR-004: temporal effect windows](adr/004-temporal-effect-windows.md)
- [Limitations](limitations.md)
