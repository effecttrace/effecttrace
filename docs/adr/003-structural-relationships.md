# ADR-003: Structural relationships

Status: Accepted

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/art/rollout-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="../assets/art/rollout-light.svg">
  <img alt="Deployment, ReplicaSets and Pods connected by controller ownership" src="../assets/art/rollout-light.svg" width="100%">
</picture>

## Context

Most effects of a Kubernetes change are made by controllers, not by the
request itself: a Deployment patch causes the Deployment controller to create
a ReplicaSet, which causes the ReplicaSet controller to create Pods, and so on.
Kubernetes records this in `ownerReferences`. Other relationships exist only
by convention or configuration: label selectors (Service to Pods through
EndpointSlices), volume references (ConfigMap to Pods), HPA targets, operator
logic.

Names are not identities: an object can be deleted and recreated with the same
name and a new UID.

## Decision

1. Follow only **controller** `ownerReferences` (`controller: true`), matched
   by **UID**, up to four levels deep.
2. Starting from each object an action changed, attribute descendants that
   were created, deleted or rescaled inside the mutation's window, subject to
   the single-claimant rule ([evidence model](../evidence-model.md#the-single-claimant-rule)).
3. Treat two further patterns as structural: Pods created by the controller
   owner of a Pod the action deleted (replacements), and descendants deleted
   after the action deleted their owner (garbage collection).
4. Keep unchanged intermediate owners in the graph as context edges
   (`controller-owner-of-directly-mutated-object`) so the ownership path is
   explicit; context edges are not claims.
5. Do not follow selectors, volume references, HPA targets or any other
   convention-based relationship in v0.1.

## Consequences

- Structural evidence is authoritative: it comes from the API server's own
  records and cannot be produced by a telemetry producer.
- Effects through Services, ConfigMaps, HPAs and operators are not shown as
  structural. A Service selector change gets only a `DIRECT` edge (L09); a
  ConfigMap change has no fan-out (L08). These gaps are documented in
  [limitations](../limitations.md) and on the [roadmap](../../ROADMAP.md).
- UID matching prevents false continuity across delete-and-recreate (L22).
- The watched kinds are limited to those on ownership paths (workloads, Pods)
  plus Services and Events, which keeps RBAC and data small.

## Alternatives considered

- **Follow label selectors.** Rejected for v0.1: selectors describe intent,
  not history; a Pod can match a selector without being affected by the
  change. EndpointSlice-based relationships are planned with their own
  evidence rules.
- **Follow all ownerReferences, not only controllers.** Rejected: non-
  controller owners are used for garbage collection relationships that do not
  imply reconciliation by the owner.
- **Match by name.** Rejected: names are reused; only UIDs are identities.
- **Unlimited depth.** Rejected: bounded depth limits cost and protects
  against malformed ownership chains.
