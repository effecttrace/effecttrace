#!/usr/bin/env bash
# EffectTrace kind lab.
#
# Safety rules enforced here:
#   * the only cluster this script creates, uses or deletes is "effecttrace-lab";
#   * kind writes its credentials to .lab/kubeconfig, so the user's default
#     kubeconfig and current context are never modified;
#   * every kubectl call passes --kubeconfig .lab/kubeconfig and
#     --context kind-effecttrace-lab explicitly.
set -euo pipefail

CLUSTER="effecttrace-lab"
CTX="kind-${CLUSTER}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LAB="${REPO}/.lab"
KCFG="${LAB}/kubeconfig"
KIND="${KIND:-${REPO}/.bin/kind}"
KIND_VERSION="v0.33.0"

log() { printf '\033[1m[lab]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m[lab] %s\033[0m\n' "$*" >&2; exit 1; }

engine() {
  if [[ -n "${CONTAINER_ENGINE:-}" ]]; then echo "${CONTAINER_ENGINE}"; return; fi
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then echo docker; return; fi
  if command -v podman >/dev/null 2>&1; then echo podman; return; fi
  die "no container engine found (docker or podman required)"
}

setup_provider() {
  if [[ "$(engine)" == podman ]]; then export KIND_EXPERIMENTAL_PROVIDER=podman; fi
}

ensure_kind() {
  if [[ ! -x "${KIND}" ]]; then
    log "installing kind ${KIND_VERSION} into .bin/"
    GOBIN="${REPO}/.bin" go install "sigs.k8s.io/kind@${KIND_VERSION}"
  fi
}

kc() { kubectl --kubeconfig "${KCFG}" --context "${CTX}" "$@"; }

cluster_exists() {
  "${KIND}" get clusters 2>/dev/null | grep -qx "${CLUSTER}"
}

guard() {
  cluster_exists || die "cluster ${CLUSTER} does not exist; run make demo-up"
  [[ -f "${KCFG}" ]] || "${KIND}" export kubeconfig --name "${CLUSTER}" --kubeconfig "${KCFG}" >/dev/null
  local server
  server="$(kubectl --kubeconfig "${KCFG}" config view -o jsonpath="{.clusters[?(@.name==\"${CTX}\")].cluster.server}")"
  [[ "${server}" == https://127.0.0.1:* ]] || die "refusing: context ${CTX} does not point at a local kind API server (${server})"
}

arch() {
  local raw=""
  case "$(engine)" in
    podman) raw="$(podman info --format '{{.Host.Arch}}' 2>/dev/null || true)" ;;
    docker) raw="$(docker info --format '{{.Architecture}}' 2>/dev/null || true)" ;;
  esac
  [[ -n "${raw}" ]] || raw="$(uname -m)"
  case "${raw}" in
    arm64|aarch64) echo arm64 ;;
    amd64|x86_64) echo amd64 ;;
    *) die "unsupported architecture ${raw}" ;;
  esac
}

build_images() {
  local a eng ver commit ldflags
  a="$(arch)"; eng="$(engine)"
  ver="${VERSION:-dev}"; commit="$(git -C "${REPO}" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  ldflags="-s -w -X github.com/effecttrace/effecttrace/internal/version.Version=${ver} -X github.com/effecttrace/effecttrace/internal/version.Commit=${commit}"
  log "building linux/${a} binaries"
  mkdir -p "${REPO}/bin/linux-${a}"
  for target in ./cmd/effecttrace-collector ./cmd/effecttrace ./demo/cmd/shopsim ./demo/cmd/mcp-tools; do
    (cd "${REPO}" && CGO_ENABLED=0 GOOS=linux GOARCH="${a}" go build -trimpath -ldflags "${ldflags}" -o "bin/linux-${a}/$(basename "${target}")" "${target}")
  done
  log "building images with ${eng}"
  "${eng}" build -q -f "${REPO}/deploy/images/Containerfile.collector" --build-arg TARGETARCH="${a}" -t localhost/effecttrace/collector:dev "${REPO}" >/dev/null
  "${eng}" build -q -f "${REPO}/deploy/images/Containerfile.demo" --build-arg TARGETARCH="${a}" -t localhost/effecttrace/demo:dev "${REPO}" >/dev/null
  # Same content under a second tag, used by image-change experiments.
  "${eng}" tag localhost/effecttrace/demo:dev localhost/effecttrace/demo:v2
  mkdir -p "${LAB}"
  rm -f "${LAB}/images.tar"
  if [[ "${eng}" == podman ]]; then
    podman save -q --format docker-archive -o "${LAB}/images.tar" localhost/effecttrace/collector:dev localhost/effecttrace/demo:dev localhost/effecttrace/demo:v2
  else
    docker save -o "${LAB}/images.tar" localhost/effecttrace/collector:dev localhost/effecttrace/demo:dev localhost/effecttrace/demo:v2
  fi
  "${KIND}" load image-archive "${LAB}/images.tar" --name "${CLUSTER}" >/dev/null
  rm -f "${LAB}/images.tar"
}

up() {
  ensure_kind; setup_provider
  mkdir -p "${LAB}/shared"
  # The collector writes its recording here as UID 0 without capabilities, so
  # it cannot rely on DAC override; on Linux hosts the directory is owned by
  # the invoking user. Lab-only: world-writable with the sticky bit.
  chmod 1777 "${LAB}/shared"
  if cluster_exists; then
    log "cluster ${CLUSTER} already exists"
  else
    log "creating kind cluster ${CLUSTER}"
    sed -e "s#__LAB_DIR__#${LAB}#g" -e "s#__REPO_DIR__#${REPO}#g" "${REPO}/deploy/kind/kind-config.yaml.tmpl" > "${LAB}/kind-config.yaml"
    "${KIND}" create cluster --name "${CLUSTER}" --config "${LAB}/kind-config.yaml" --kubeconfig "${KCFG}" --wait 120s
  fi
  guard
  build_images
  log "deploying workload, observability stack, EffectTrace and demo actor"
  kc apply -f "${REPO}/deploy/lab/shop.yaml" >/dev/null
  kc apply -f "${REPO}/deploy/lab/observability.yaml" >/dev/null
  kc apply -f "${REPO}/deploy/collector/collector.yaml" >/dev/null
  # A stable pseudonymization key, generated once and kept only in the cluster.
  if ! kc -n effecttrace-system get secret effecttrace-identity >/dev/null 2>&1; then
    kc -n effecttrace-system create secret generic effecttrace-identity \
      --from-literal=key="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" >/dev/null
  fi
  kc apply -f "${REPO}/deploy/lab/collector-nodeport.yaml" >/dev/null
  kc apply -f "${REPO}/deploy/lab/demo-actor.yaml" >/dev/null
  # Restart our own components so freshly loaded images are used.
  kc -n effecttrace-system rollout restart deployment/effecttrace-collector >/dev/null
  kc -n effecttrace-demo rollout restart deployment/mcp-tools >/dev/null
  for d in shop/frontend shop/checkout shop/payments shop/inventory shop/loadgen observability/otel-collector observability/prometheus effecttrace-system/effecttrace-collector effecttrace-demo/mcp-tools; do
    if ! kc -n "${d%%/*}" rollout status "deployment/${d##*/}" --timeout=240s >/dev/null; then
      log "deployment ${d} did not become ready; pods and recent events:"
      kc -n "${d%%/*}" get pods -o wide >&2 || true
      for p in $(kc -n "${d%%/*}" get pods -o name 2>/dev/null); do
        kc -n "${d%%/*}" logs "${p}" --previous --tail=20 >&2 2>/dev/null || kc -n "${d%%/*}" logs "${p}" --tail=20 >&2 2>/dev/null || true
      done
      kc -n "${d%%/*}" get events --sort-by=.lastTimestamp 2>/dev/null | tail -15 >&2 || true
      die "lab setup failed"
    fi
  done
  log "lab is ready"
  log "  EffectTrace API     http://127.0.0.1:18080"
  log "  MCP tool server     http://127.0.0.1:18081/mcp"
  log "  OTLP (agent spans)  http://127.0.0.1:14318"
  log "  Prometheus          http://127.0.0.1:19090"
}

down() {
  ensure_kind; setup_provider
  if cluster_exists; then
    log "deleting kind cluster ${CLUSTER} (and only that cluster)"
    "${KIND}" delete cluster --name "${CLUSTER}" --kubeconfig "${KCFG}"
  else
    log "cluster ${CLUSTER} does not exist"
  fi
  rm -f "${KCFG}"
}

status() {
  ensure_kind; setup_provider; guard
  kc get pods -A -o wide
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  status) status ;;
  images) ensure_kind; setup_provider; guard; build_images ;;
  kubectl) shift; ensure_kind; setup_provider; guard; kc "$@" ;;
  *) echo "usage: $0 {up|down|status|images|kubectl ...}" >&2; exit 2 ;;
esac
