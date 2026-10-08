# Security model

EffectTrace is an observability system, not an authorization system. It
records evidence-graded relationships after the fact; it never approves,
blocks or rate-limits actions. Deciding who may do what remains the job of
Kubernetes RBAC, admission control and whatever gates your agents.

This page describes the trust boundaries, the privileges each component holds,
and how to deploy the collector safely. The threats and mitigations are listed
in the [threat model](threat-model.md).

## Principles

1. **Observer and actor are separate.** The collector only reads. Anything
   that mutates the cluster (an MCP tool server, a human, a controller) holds
   its own, separate rights.
2. **Least privilege.** The collector can get, list and watch the kinds it
   correlates, and nothing else.
3. **Minimize data at the source.** Fields EffectTrace does not use are
   dropped before they are cached or stored ([privacy](privacy.md)).
4. **Untrusted input is bounded and validated.** OTLP input in particular is
   treated as untrusted.
5. **Uncertainty is shown, not hidden.** Coverage, ambiguities and exclusions
   are part of every graph.

## Observer RBAC

From [`deploy/collector/collector.yaml`](../deploy/collector/collector.yaml):

| API group | Resources | Verbs |
| --- | --- | --- |
| `apps` | deployments, replicasets, statefulsets, daemonsets | get, list, watch |
| `batch` | jobs | get, list, watch |
| core | pods, services, events | get, list, watch |

There are no mutation verbs and no access to ConfigMaps, Secrets, nodes,
RBAC objects or subresources such as `pods/exec`. To limit the watch to
specific namespaces, set `--namespaces` and replace the ClusterRoleBinding with
per-namespace RoleBindings.

## Demo actor RBAC

The lab's MCP tool server is a separate **actor** with its own service account
(`effecttrace-demo/demo-actor`), defined in
[`deploy/lab/demo-actor.yaml`](../deploy/lab/demo-actor.yaml). Its Role is
namespace-scoped to `shop`:

| API group | Resources | Verbs |
| --- | --- | --- |
| `apps` | deployments, deployments/scale | get, patch |
| core | pods | get, delete |
| core | configmaps, services | get, patch |

The tool server additionally refuses namespaces outside `ALLOWED_NAMESPACES`
and validates every input. It runs as non-root (UID 65532) with a read-only
root filesystem, all capabilities dropped and the `restricted` Pod Security
level enforced on its namespace.

These rights belong to the demo, not to EffectTrace. Nothing in EffectTrace
needs them.

## Reading the audit log: the UID 0 tradeoff

kube-apiserver writes its audit log as root with mode `0600`. To read it, the
lab collector:

- runs as **UID 0**, but with **every Linux capability dropped**,
  `allowPrivilegeEscalation: false`, a read-only root filesystem and the
  `RuntimeDefault` seccomp profile;
- mounts only the audit log directory, **read-only**, through a `hostPath`;
- is pinned to the control-plane node, where kube-apiserver writes the log;
- runs in the `effecttrace-system` namespace, which must allow the
  `privileged` Pod Security level because of the `hostPath` volume (it warns
  at `baseline`).

Without capabilities, root in the container cannot bypass file permissions
beyond ownership (no `CAP_DAC_OVERRIDE`), so it can read files owned by root
but cannot load modules, change ownership or open raw sockets. It is still
more privilege than a non-root process, and a container escape would start as
root. Alternatives, each with its own tradeoff:

| Option | Effect |
| --- | --- |
| Run without `--audit-log` | The collector runs as non-root. Tool calls keep `DIRECT` evidence from the instrumented client's response metadata, but audit confirmation and audit-only actions (humans, controllers configured as actors) are lost (lab experiment L27). |
| Have a node-level log agent copy the audit log to a file readable by a dedicated non-root group | The collector runs non-root; the copy adds latency and another component to trust. |
| On managed control planes, export the provider's audit stream to a JSON-lines file the collector can read | Depends on the provider; EffectTrace v0.1 reads only the log backend's JSON-lines format. |

## Network exposure

| Port | Purpose | Authentication | Recommendation |
| --- | --- | --- | --- |
| API `:8080` | `/api/v1/*`, `/healthz`, `/readyz`, `/metrics` | optional bearer token on `/api/*` only | expose only inside the cluster; publish through an ingress or proxy that terminates TLS and authenticates users |
| OTLP `:4318` | span ingest | none | allow only your OpenTelemetry Collector(s) with a NetworkPolicy |
| outbound | Prometheus queries, OTLP export, Kubernetes API | as configured | operator-configured URLs only |

The collector has **no built-in TLS** and no user authorization. The bearer
token (`--api-token-file`) is a single shared secret compared in constant
time; the CLI sends it from `EFFECTTRACE_TOKEN`. Graph export uses TLS unless
`--otlp-export-insecure` is set.

The generic manifest exposes the collector through a ClusterIP Service and
ships a NetworkPolicy that admits OTLP only from the `observability`
namespace. The lab adds a NodePort (`deploy/lab/collector-nodeport.yaml`) that
kind maps to `127.0.0.1` of the host only. Do not reuse the lab Service, the
missing token or plain HTTP export outside a local lab.

An example NetworkPolicy that admits only the `observability` namespace (where
the OpenTelemetry Collector and Prometheus run). Add rules for whatever
legitimately queries the API, such as your ingress controller:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: effecttrace-collector
  namespace: effecttrace-system
spec:
  podSelector:
    matchLabels: { app.kubernetes.io/name: effecttrace-collector }
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels: { kubernetes.io/metadata.name: observability }
      ports:
        - { port: 4318, protocol: TCP }
        - { port: 8080, protocol: TCP }
```

## Container hardening

The collector image is built `FROM scratch` with statically linked binaries
(`CGO_ENABLED=0`) and declares `USER 65532:65532`; the lab Deployment overrides
the user to 0 only for audit log access, as described above. HTTP servers set
read-header and read timeouts; the API also sets a write timeout. API responses
carry `X-Content-Type-Options: nosniff`, `Cache-Control: no-store` and
`Content-Security-Policy: default-src 'none'`.

## Kubernetes access safety

- `--kube=kubeconfig` requires an explicit `--kube-context`; EffectTrace never
  uses the current context implicitly.
- The lab scripts create, use and delete only the `effecttrace-lab` kind
  cluster, write its credentials to `.lab/kubeconfig`, pass `--kubeconfig` and
  `--context` on every call, and refuse to run when the context does not point
  at `https://127.0.0.1:*`. See [deployment](deployment.md#local-lab).

## Supply chain

- Go modules are pinned in `go.mod`/`go.sum` and updated by the maintainer;
  `govulncheck` runs in CI on every change.
- GitHub Actions are pinned by commit SHA.
- CI runs `golangci-lint` (including `gosec`), `go vet`, unit tests with the
  race detector, integration tests, fuzzing, `govulncheck`, CodeQL and OpenSSF
  Scorecard.
- Lab images are pinned by digest.
- `make scan` checks tracked files and commit metadata for personal data,
  credentials and private paths before pushing.

See [DEPENDENCIES.md](../DEPENDENCIES.md) and [releasing](releasing.md).

## Reporting vulnerabilities

See [SECURITY.md](../SECURITY.md).
