# ADR-004: Temporal effect windows

Status: Accepted

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/art/windows-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="../assets/art/windows-light.svg">
  <img alt="Baseline, reconciliation and telemetry windows" src="../assets/art/windows-light.svg" width="100%">
</picture>

## Context

Ownership tells EffectTrace *which* objects could be affected by an action; it
does not tell *when* to stop attributing. A Deployment keeps its ReplicaSets
for its whole life, and later actions on the same Deployment cause more
changes through the same ownership path. Telemetry needs a bounded interval
too, and a baseline to compare with.

A fixed window (for example "60 seconds after the request") is too long for a
scale-up that settles in seconds and too short for a slow rollout.

## Decision

1. Each changed mutation gets a **reconciliation window** from the request time
   minus a skew tolerance (500 ms) until the first stable status of the
   changed workload plus a settle period (5 s), capped at a maximum (3 min).
2. "Stable" is evaluated from the watch per kind: for a Deployment,
   `observedGeneration` reached the mutation's generation and updated,
   available and current replicas equal desired replicas (or scaled to zero);
   for a ReplicaSet, ready equals desired; for a StatefulSet, ready and updated
   equal desired; for a Pod deletion, the owner reports ready equal to
   desired; otherwise when the change was observed.
3. While a window is open the graph is `OBSERVING`. A capped window states
   that the workload did not report a stable status.
4. **Telemetry** is evaluated once per action, after all reconciliation
   windows closed and a further 15 s telemetry settle period passed, over
   `[first request, last window end + 15 s]`, against a baseline of the 60 s
   before the first request.
5. Telemetry changes are only ever `TEMPORAL_CORRELATION`.
6. `--max-reconcile`, `--settle` and `--baseline` are configurable; the skew
   tolerance and telemetry settle are code defaults.

## Consequences

- Windows adapt to the actual rollout, so fast actions complete quickly and
  slow rollouts are followed to the end.
- A rollout that never stabilizes is attributed until the cap; the lab uses
  `--max-reconcile=90s` to keep failed-rollout experiments short (L06).
- Effects after the window are missed by design. R05 replays with a very short
  window (lower recall, no false attachments); R06 with a very long one
  (overlaps become ambiguities, not false attachments).
- The single-claimant rule depends on windows: overlapping windows of actions
  on the same workload produce ambiguities.
- Stable-status detection reads replica counters, not Deployment conditions;
  a controller that never updates `observedGeneration` would keep windows
  open until the cap.

## Alternatives considered

- **Fixed-length windows.** Rejected: wrong for most rollouts in one direction
  or the other.
- **Deployment `Progressing`/`Available` conditions.** Not used in v0.1:
  replica counters and `observedGeneration` give the same signal for the kinds
  covered and also work for ReplicaSets and StatefulSets.
- **Windows ending at the next action on the same workload.** Rejected: it
  would silently assign shared effects to whichever action came first.
  Ambiguity is more honest.
- **Continuous telemetry correlation.** Rejected: evaluating once, after the
  windows closed, makes completed graphs stable (L21).
