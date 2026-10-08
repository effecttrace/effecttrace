# Deployment

EffectTrace v0.1 is evaluated on a local kind lab. There is no published
container image and no Helm chart yet ([roadmap](../ROADMAP.md)). This page
explains the lab, then how to adapt the manifests to another cluster.

## Local lab

The lab is a single-node kind cluster named `effecttrace-lab` with:

- the `shop` namespace: a small fictional shop (frontend, checkout, payments,
  inventory, loadgen), all running the demo `shopsim` binary;
- the `observability` namespace: OpenTelemetry Collector 0.162.0 (core
  distribution) and Prometheus v3.15.0, both unmodified and pinned by digest;
- the `effecttrace-system` namespace: the EffectTrace collector;
- the `effecttrace-demo` namespace: the demo MCP tool server (the actor).

### Requirements

- Go (the version in `go.mod`)
- Docker or Podman
- `kubectl`
- kind v0.33.0 (installed automatically into `.bin/` if missing)

### Commands

```sh
make demo-up      # create the cluster, build and load images, deploy everything
make demo-run     # scripted demo (see docs/demo.md)
make test-e2e     # experiment suite (see docs/testing.md)
make images       # rebuild and reload images into the running lab
./scripts/lab.sh status            # pods in all namespaces
./scripts/lab.sh kubectl -n shop get deploy
make demo-down    # delete the effecttrace-lab cluster, and only that cluster
```

`make demo-up` renders [`deploy/kind/kind-config.yaml.tmpl`](../deploy/kind/kind-config.yaml.tmpl)
into `.lab/kind-config.yaml`, creates the cluster with the default kind
v0.33.0 node image (`kindest/node:v1.37.0`, pinned by digest), builds static
`linux/<arch>` binaries and the images `localhost/effecttrace/collector:dev`
and `localhost/effecttrace/demo:dev` (plus a `demo:v2` tag used by
image-change experiments), loads them into the cluster, applies the manifests
and waits for every rollout.

### Lab safety

[`scripts/lab.sh`](../scripts/lab.sh) enforces these rules:

- the only cluster it creates, uses or deletes is `effecttrace-lab`;
- kind writes the cluster's credentials to `.lab/kubeconfig` (git-ignored), so
  your default kubeconfig and current context are **never modified**;
- every `kubectl` call passes `--kubeconfig .lab/kubeconfig` and
  `--context kind-effecttrace-lab` explicitly;
- before acting, it checks that the context's API server is
  `https://127.0.0.1:*` and refuses otherwise.

The experiment harness applies the same check before it connects.

### Podman

The lab picks the container engine automatically: Docker if `docker info`
succeeds, otherwise Podman. Set `CONTAINER_ENGINE=podman` (or `docker`) to
choose explicitly. With Podman, the script sets
`KIND_EXPERIMENTAL_PROVIDER=podman` for kind and saves images with
`podman save --format docker-archive` before loading them. On macOS, the
Podman machine needs enough memory for kind, and its clock must stay in sync
with the host for the experiments (see [troubleshooting](troubleshooting.md)).

### Lab endpoints

All ports are bound to `127.0.0.1` on the host only.

| Endpoint | Host address | In-cluster |
| --- | --- | --- |
| EffectTrace API, `/metrics`, health | `http://127.0.0.1:18080` | `effecttrace.effecttrace-system.svc:8080` |
| Demo MCP tool server | `http://127.0.0.1:18081/mcp` | `mcp-tools.effecttrace-demo.svc:8080` |
| Shop fault relay (experiments) | `http://127.0.0.1:18082` | `loadgen.shop.svc:8080` |
| OpenTelemetry Collector OTLP/HTTP (application spans) | `http://127.0.0.1:14318` | `otel-collector.observability.svc:4318` |
| Prometheus | `http://127.0.0.1:19090` | `prometheus.observability.svc:9090` |
| EffectTrace OTLP receiver | not exposed | `effecttrace.effecttrace-system.svc:4318` |
| OpenTelemetry Collector graph receiver | not exposed | `otel-collector.observability.svc:4319` |

Trace flow in the lab: agent and tool server -> OpenTelemetry Collector
(`otlp/apps`) -> EffectTrace receiver. EffectTrace exports completed graphs to
the Collector's separate `otlp/graphs` receiver, which sends them to the
`debug` exporter, so graphs never loop back.

### Lab collector configuration

From [`deploy/collector/collector.yaml`](../deploy/collector/collector.yaml):

```text
--api-listen=:8080
--otlp-listen=:4318
--audit-log=/var/log/kubernetes/audit/audit.log
--kube=in-cluster
--prometheus-url=http://prometheus.observability.svc:9090
--signals=/etc/effecttrace/signals.yaml
--record=/var/lib/effecttrace/recording.jsonl
--otlp-export-endpoint=otel-collector.observability.svc:4319
--otlp-export-insecure
--max-reconcile=90s        # lab value; the default is 3m
```

The recording directory is shared with the host as `.lab/shared/` so replay
experiments can read it.

## Deploying to another cluster

The manifests are a starting point, not a production package. Review each item
below.

### 1. Build and publish the image

```sh
ARCH=amd64
mkdir -p bin/linux-${ARCH}
for t in effecttrace-collector effecttrace; do
  CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/effecttrace/effecttrace/internal/version.Version=${VERSION:-dev}" \
    -o bin/linux-${ARCH}/${t} ./cmd/${t}
done
docker build -f deploy/images/Containerfile.collector --build-arg TARGETARCH=${ARCH} \
  -t registry.example.com/effecttrace/collector:${VERSION:-dev} .
```

The image is `FROM scratch` and contains only the two binaries.

### 2. Enable the audit log (optional but recommended)

EffectTrace reads the kube-apiserver **log backend** in JSON lines. Configure
kube-apiserver with, for example:

```text
--audit-policy-file=/etc/kubernetes/policies/audit-policy.yaml
--audit-log-path=/var/log/kubernetes/audit/audit.log
--audit-log-maxsize=50
--audit-log-maxbackup=2
```

and a policy like [`deploy/kind/audit-policy.yaml`](../deploy/kind/audit-policy.yaml)
(`Metadata` level, no reads, no leases or Events). The tailer follows
rotation. By default it reads the file from its beginning at startup, which
re-ingests history after a restart (duplicates are ignored); use
`--audit-from-end` to start at the end instead.

Reading the file requires running on the control-plane node with a read-only
`hostPath` mount and, because the file is root-owned with mode `0600`, as UID
0 without capabilities. Review the tradeoff and alternatives in the
[security model](security-model.md#reading-the-audit-log-the-uid-0-tradeoff).
Without `--audit-log`, the collector runs as non-root.

### 3. Apply the observer RBAC

Use the ClusterRole in `collector.yaml` as is. To restrict the watch, pass
`--namespaces=a,b` and bind the role per namespace with RoleBindings.

### 4. Configure sources and outputs

- **Traces:** point an OpenTelemetry Collector `otlp_http` exporter at
  `http://<service>:4318` ([OpenTelemetry](opentelemetry.md)).
- **Signals:** write a signals file for your metrics and pass
  `--prometheus-url` and `--signals` ([Prometheus](prometheus.md)).
- **Graph export:** `--otlp-export-endpoint host:port`; omit
  `--otlp-export-insecure` to use TLS.
- **Recording:** `--record` and `--record-max-bytes` if you want offline
  replay; protect the file.

### 5. Protect identities and the API

```sh
kubectl -n effecttrace-system create secret generic effecttrace-identity \
  --from-literal=key="$(openssl rand -hex 32)"
openssl rand -hex 32 > token && kubectl -n effecttrace-system create secret generic effecttrace-api-token --from-file=token
```

Mount the token Secret and pass `--api-token-file`. The manifest in
`deploy/collector/collector.yaml` already uses a ClusterIP Service and a
NetworkPolicy (enforced only if your CNI supports NetworkPolicy); do not apply
the lab-only `deploy/lab/collector-nodeport.yaml`. Expose the API only
through an ingress or proxy that terminates TLS and authenticates users
([security model](security-model.md#network-exposure)).

### 6. Size and run one replica

Keep `replicas: 1` and `strategy: Recreate`. The lab requests 50m CPU and
64 MiB memory with limits of 1 CPU and 512 MiB; size from your own observation
volume, `--retention` and `--queue-size`, and watch
`effecttrace_queue_depth` and process memory.

## Health and readiness

| Path | Meaning |
| --- | --- |
| `/healthz` | the process is serving |
| `/readyz` | Kubernetes informers have synced (always ready with `--kube=none`) |

## Running outside Kubernetes

The collector can run on a workstation against a cluster you choose
explicitly:

```sh
./bin/effecttrace-collector --kube=kubeconfig --kubeconfig ~/.kube/config \
  --kube-context my-test-cluster --otlp-listen=:4318 --api-listen=127.0.0.1:8080
```

`--kube-context` is required with `--kube=kubeconfig`; EffectTrace never uses
the current context implicitly. `--kube=none` runs with OTLP and audit input
only (no watch, so no structural evidence).

## Related documents

- [CLI reference](cli.md)
- [Security model](security-model.md)
- [Troubleshooting](troubleshooting.md)
