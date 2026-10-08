# ADR-008: No-op requests and change-level claims

Status: Accepted

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/art/concurrent-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="../assets/art/concurrent-light.svg">
  <img alt="Two actions on the same workload: shared changes become ambiguities" src="../assets/art/concurrent-light.svg" width="100%">
</picture>

## Context

Three situations produce false attachments if they are handled naively.

**No-op requests.** A successful request does not always change anything: a
`kubectl apply` of an unchanged manifest, a patch that sets a field to its
current value. If every successful request opened a reconciliation window and
claimed descendants, a no-op apply made while another action's rollout was in
progress would compete for, or wrongly claim, that rollout's effects.

**Object-level claims.** An object can be created by one action and deleted
by another: a Pod created by a scale-up is terminated by a later restart. If
claims were made per object, the second action would either inherit or block
the first action's claim.

**Temporal links.** When a tool call reaches a Kubernetes request only through
timing, letting the tool call own the request's controller effects would turn
a correlation into an attribution.

## Decision

1. **Only observed changes carry claims.** A mutation claims controller
   fan-out only when the watch observed its change: for generation-bearing
   kinds (Deployment, ReplicaSet, StatefulSet, DaemonSet, Job) an increase of
   `metadata.generation`; for creates the object's first observation; for
   deletes a DELETED notification or a set `deletionTimestamp`.
2. **The observation is bound to the request.** It is matched by the
   resourceVersion the instrumented client reported, or else taken from the
   first notification between the request (minus skew tolerance) and one
   second after the request completed (plus the settle period for deletes).
   A later notification may belong to a later request and is not used.
3. A request without an observed change keeps its `DIRECT` edge, marks the
   object `UNCHANGED`, gets a short window `[request - skew, request +
   settle]`, and never claims later effects.
4. **Claims are per change, not per object.** `CREATED`, `DELETED` and
   `SCALED` of one object are separate changes with separate claimants. When
   the same action both created and deleted an object, the node records
   `CREATED` with `deletedAt`.
5. **Direct claims take precedence.** If an action directly changed the object
   with the same change type in the relevant interval, only direct claimants
   count for that change.
6. **Temporal links grant no ownership.** Mutations reached through
   `request-received-during-tool-span` are not indexed as the tool call's
   claims; the request's own API-call action holds them. The tool call's graph
   treats that action's claims as its own when deciding, so its view shows the
   effects, graded `CORRELATED`.

## Consequences

- No-op requests never create false attachments or spurious ambiguities
  (unit test `TestNoOpRequestNeverClaimsLaterEffects`).
- Sequences such as "scale up, then restart" attribute each Pod's creation and
  termination to the action responsible for each change (scenario L33 checks
  that each action owns only its own Pods).
- A real change whose watch notification arrives more than one second after
  request completion is treated as unobserved for an audit-only request; the
  graph shows `UNCHANGED` and no fan-out. With an instrumented client the
  resourceVersion match removes this risk.
- Status-only updates by controllers never count as the action's change.

## Alternatives considered

- **Treat every successful request as a change.** Rejected: produces false
  claims for no-op applies.
- **Compare object specs before and after.** Rejected: requires retaining
  specs, which conflicts with [ADR-006](006-privacy-defaults.md).
- **Per-object claims.** Rejected: cannot represent objects created and
  deleted by different actions.
- **Let temporal links own effects.** Rejected: would upgrade correlation to
  attribution.
