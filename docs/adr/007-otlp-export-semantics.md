# ADR-007: OTLP export semantics

Status: Accepted

## Context

Operators already use a tracing backend. Showing EffectTrace's result there,
next to the agent's trace, is more useful than another UI. The question is how
to represent an effect graph in OTLP without misrepresenting it.

Options include appending child spans to the agent's trace, creating one span
per node or edge, or emitting one span per graph.

The controller-driven effects are asynchronous: the tool span ends when the
API request returns, long before the rollout finishes. The OpenTelemetry
specification recommends span links, not parent/child, for relationships where
the parent does not enclose the work.

## Decision

1. Export each graph **once**, when it first reaches `COMPLETE`, as **one span**
   named `effecttrace effect_graph` (kind `INTERNAL`) in a **new trace**.
2. Add a **span link** to the initiating tool span, with
   `effecttrace.link.type = initiating_action`. Never make the graph span a
   child of the tool span, and never write spans into the application's trace.
3. Record the graph identity, status, action and per-evidence edge counts as
   span attributes, and each edge as an `effecttrace.edge` **span event**
   (relationship, evidence, from, to, bounded reason, target node type,
   target UID), at most 128 per span with a truncation count.
4. Use the `effecttrace.*` attribute namespace; do not reuse `k8s.*` resource
   attributes for targets ([semantic conventions](../semantic-conventions.md)).
5. Use OTLP/HTTP with TLS unless `--otlp-export-insecure` is set; the
   resource is `service.name = effecttrace`.

## Consequences

- The agent's trace stays exactly as its producers wrote it. Backends that
  render links let users jump from the tool span to the graph span.
- A graph is one span, so it does not inflate trace sizes or span counts.
- Edge events are a flattened view; the full graph (nodes, ambiguities,
  exclusions, coverage) is available only through the API or JSON export.
- In a deployment where the OpenTelemetry Collector forwards everything to
  EffectTrace, graph spans must use a separate pipeline to avoid feeding them
  back (the lab uses a second receiver on port 4319). EffectTrace would ignore
  them anyway, because they carry neither `mcp.method.name` nor
  `effecttrace.k8s.verb`.
- Graphs that never complete are not exported.

## Alternatives considered

- **Child spans under the tool span.** Rejected: misrepresents asynchronous
  effects as enclosed work, and mutates traces owned by other producers.
- **One span per node or edge.** Rejected: large, hard to read in most
  backends, and node identity does not map well to spans.
- **Exporting while observing and updating later.** Rejected: spans are
  immutable once ended; exporting partial graphs would leave stale versions in
  the backend.
- **OTLP log records or events.** Deferred: a span with a link is
  the most widely supported way to make the result navigable from the
  initiating trace today.
