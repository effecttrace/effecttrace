# ADR-002: Direct action correlation

Status: Accepted

## Context

To say "this tool call changed this Deployment" with `DIRECT` evidence,
EffectTrace needs identifiers that connect three independent observations:
the tool call's span, the Kubernetes API request, and the object that changed.

Candidate identifiers:

- **Trace context.** OpenTelemetry parent/child connects the tool span to the
  client span of the Kubernetes request, if the client is instrumented.
- **The audit ID.** kube-apiserver assigns every request an audit ID, writes
  it to the audit event and returns it in the `Audit-Id` response header. But
  kube-apiserver also accepts a client-supplied `Audit-ID` request header
  without validation (kubernetes/kubernetes issues #127801 and #101597), and
  the behaviour is undocumented.
- **Object identity.** The response to a mutating request contains the
  object's UID, resourceVersion and generation. The audit event's
  `objectRef.uid` is filled only from a decoded request object, so it is empty
  for PATCH and DELETE.
- **kube-apiserver tracing.** The API server uses an incoming `traceparent` as
  the parent only for privileged callers, and audit events carry no trace ID.

## Decision

1. Provide a client-go transport wrapper, `pkg/k8sinstrument`, that records on
   the client span of each mutating request: the request target, the
   **server-generated** Audit-ID from the response header, and the response
   object's kind, UID, resourceVersion and generation, read from JSON or
   Kubernetes protobuf metadata without recording bodies.
2. **Never set** the `Audit-ID` request header.
3. Use an audit event as confirmation of a client span only when it **agrees**
   on verb, resource, subresource, namespace and name. On disagreement, ignore
   the audit event and add a note.
4. Resolve the target UID in order: response metadata, audit `objectRef.uid`,
   then the unique object with that kind, namespace and name at request time.
   Record which rule was used on the edge. Refuse to assert a UID when the name
   is ambiguous.
5. Discover audit-only mutating requests (not claimed by a tool call, not made
   by controller identities, not on ignored resources, not dry-run) as their
   own `KUBERNETES_API_CALL` actions, so human and controller-as-actor changes
   are investigated too.
6. When a tool call has no trace-linked request, optionally connect requests
   received during the tool span with `TEMPORAL_CORRELATION` only, without
   giving the tool call ownership of their effects.

## Consequences

- With an instrumented client, `DIRECT` evidence survives a missing audit log
  (lab experiment L27), and audit confirmation adds an independent check.
- Audit-only actions depend on the watch for name resolution; deleted-and-
  recreated names are handled by refusing to guess.
- MCP servers must use the wrapper (one line with client-go) to get the
  strongest evidence. Without it, tool calls fall back to temporal
  correlation (L24).
- A forged span can still name a real audit ID, but must also match the
  request target to be accepted.

## Alternatives considered

- **Set our own Audit-ID on requests and look it up in the audit log.**
  Rejected: it depends on undocumented, unvalidated behaviour and makes
  spoofing indistinguishable from instrumentation.
- **Rely on kube-apiserver tracing.** Rejected for v0.1: the parent link is
  available only for privileged callers and audit events lack trace IDs.
- **Match by time and target name only.** Rejected as `DIRECT` evidence; kept
  only as name resolution for audit-only actions with an explicit, weaker rule
  name.
- **A mutating proxy in front of kube-apiserver.** Rejected: EffectTrace
  observes and must not sit in the request path.
