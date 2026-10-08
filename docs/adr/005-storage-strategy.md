# ADR-005: Storage strategy

Status: Accepted

## Context

EffectTrace correlates recent activity: an action and the minutes after it.
It must survive floods of hostile or merely noisy input, produce the same
graph regardless of arrival order, and allow an investigation to be repeated
offline. It is not meant to be a long-term store of traces, audit events or
metrics; those systems already exist.

## Decision

1. Keep all observations **in memory**, in one store, bounded by count per
   collection and by age (2 h retention by default). Oldest records are
   evicted first; at most 64 observations are kept per object (the first and
   the most recent).
2. Make every write an **idempotent keyed upsert** (audit ID; trace and span
   ID; UID, resourceVersion and watch type; Event UID; action, signal and
   workload). Duplicates are counted and ignored.
3. Store only **normalized, minimized** records (`internal/obs`), validated
   against explicit bounds; values over a limit are rejected, not truncated.
4. Use a **single writer** fed by one bounded queue. Sources that can wait
   block; the OTLP receiver answers 503 when the queue is full.
5. Build graphs **on demand** as a pure function of the store, the
   configuration and the current time, with a cached index per store version
   and clock second.
6. Optionally **record** every applied record as JSON lines (`--record`), and
   provide `effecttrace replay` to rebuild graphs offline from a recording.

## Consequences

- No database to operate; startup is immediate; memory is bounded.
- A restart loses spans and in-memory state. The audit log is re-read and
  objects relisted, so results after a restart are degraded but honest (L25).
- One replica only; there is no shared state for horizontal scaling.
- Determinism makes replay exact (R01) and order-independent (R02), and lets
  tests compare graphs byte for byte.
- Graphs older than the retention period disappear from the API; users who
  need them keep exported JSON or recordings.
- Graph construction cost grows with retained state; the periodic summary
  rebuilds every graph every few seconds.

## Alternatives considered

- **An embedded or external database.** Deferred: it adds operations burden
  and a schema to migrate before the evidence model is stable. A durable
  storage option is on the [roadmap](../../ROADMAP.md).
- **Storing graphs instead of observations.** Rejected: graphs change while
  windows are open and depend on configuration; storing observations keeps
  rebuilds and replays exact.
- **Unbounded in-memory state.** Rejected: one flood would take the collector
  down.
- **Reading back from existing backends (tracing backend, Prometheus, audit
  storage) at query time.** Rejected for v0.1: availability and query
  semantics vary widely; Prometheus is the only source queried at evaluation
  time.
