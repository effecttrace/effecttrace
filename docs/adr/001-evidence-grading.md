# ADR-001: Evidence grading

Status: Accepted

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/art/evidence-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="../assets/art/evidence-light.svg">
  <img alt="Evidence classes from DIRECT to TEMPORAL_CORRELATION" src="../assets/art/evidence-light.svg" width="100%">
</picture>

## Context

After an agent or a human changes a Kubernetes workload, many things happen:
the object changes, controllers create and delete ReplicaSets and Pods, Events
are emitted, metrics move. Some of these are certainly connected to the
action, some are connected through Kubernetes structure, and some merely
happened at the same time. Other things happen at the same time for unrelated
reasons, including other actions.

Tools that put all of these into one undifferentiated "related" list invite
the reader to treat coincidence as cause. That is the specific failure
EffectTrace exists to avoid. At the same time, a graph that shows only
certainties would hide useful leads such as a latency increase inside the
rollout window.

## Decision

1. Every edge states its **relationship** (what it asserts) and its
   **evidence class** (what supports it) separately. The classes are
   `DIRECT`, `TRACE_LINK`, `STRUCTURAL`, `EVENT_REFERENCE`,
   `TEMPORAL_CORRELATION`, and `INFERRED` (reserved, not emitted in v0.1).
2. Each relationship admits **exactly one** evidence class.
   `model.NewEdge` derives the class from the relationship, and
   `EffectGraph.Validate` rejects any mismatch, including in graphs read from
   files. Code cannot attach a strong class to a weak relationship.
3. Each node carries a **path grade**: the weakest evidence on the strongest
   path from the action (`ATTRIBUTED`, `CORRELATED`, `INFERRED`). Grades are
   always recomputed, never read from input.
4. `STRUCTURAL_OWNER` edges are also traversed from owned object to owner when
   computing grades, because ownership is known in both directions.
5. Every edge records the named **rule** that admitted it and a human-readable
   reason, so an operator can audit each claim.
6. Graphs carry ambiguities, exclusions and coverage alongside edges.
7. Any graph with a temporal edge carries the note "Temporal correlation does
   not prove causation."

## Consequences

- Readers and tools can filter by grade: `ATTRIBUTED` nodes are supported by
  identifiers; `CORRELATED` nodes are leads.
- Evaluation can count claims precisely: a claim is an `ATTRIBUTED` object
  node ([testing](../testing.md#metrics)).
- A missing trace link degrades a whole subtree to `CORRELATED`, which is
  correct but can surprise users who expect the controller fan-out below a
  correlated request to remain attributed.
- Adding a new kind of evidence requires a new relationship, a schema change
  and an ADR, by design.
- `INFERRED` exists in the schema so that adding heuristics later does not
  change the JSON shape, but it is unused today.

## Alternatives considered

- **A single numeric confidence score per edge.** Rejected: scores invite
  false precision, mix unrelated kinds of evidence, and are hard to audit.
  Named classes and rules are explainable.
- **Free choice of evidence class per edge.** Rejected: one coding mistake
  could present timing as direct evidence. Fixing the class by relationship
  makes that impossible.
- **Omitting temporal correlation entirely.** Rejected: operators want to know
  that a signal changed inside the window. Keeping it with an explicit weak
  class is more useful and still honest.
- **Graph-wide grade instead of per-node grades.** Rejected: a graph usually
  mixes attributed structure with correlated telemetry; per-node grades keep
  both visible.
