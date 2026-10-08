#!/usr/bin/env bash
# The EffectTrace demo, run live against the kind lab (make demo-up first).
#   1. a scripted agent restarts shop/checkout through MCP
#   2. EffectTrace explains the evidence-graded effect graph
#   3. a human scales shop/payments at the same moment: it must stay separate
#   4. two agents act on two workloads at once: their graphs must not mix
# No language model is involved; every step is scripted and reproducible.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO}"
export OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:14318"
SERVER="http://127.0.0.1:18080"
mkdir -p bin
go build -o bin/agent ./demo/cmd/agent
go build -o bin/effecttrace ./cmd/effecttrace

step() { printf '\n\033[1;36m━━ %s\033[0m\n' "$*"; }
kc() { ./scripts/lab.sh kubectl "$@"; }

wait_complete() { # trace-id or action-id
  local flag="$1" id="$2"
  for _ in $(seq 1 90); do
    if ./bin/effecttrace explain --server "${SERVER}" "${flag}" "${id}" --color never 2>/dev/null | grep -q "status COMPLETE"; then
      return 0
    fi
    sleep 2
  done
  echo "graph for ${id} did not complete in time" >&2
  return 1
}

trace_of() { sed -E 's/.*"traceId":"([0-9a-f]{32})".*/\1/' <<<"$1"; }

./bin/effecttrace doctor --server "${SERVER}"

step "1. The scripted agent calls restart_workload(shop/checkout)"
out="$(./bin/agent restart_workload namespace=shop deployment=checkout)"
echo "${out}"
trace="$(trace_of "${out}")"

step "2. EffectTrace explains what changed, and why each edge exists"
wait_complete --trace-id "${trace}"
./bin/effecttrace explain --server "${SERVER}" --trace-id "${trace}"

step "3. Agent restarts checkout while a human scales payments at the same moment"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
./bin/agent restart_workload namespace=shop deployment=checkout > "${tmp}/a.json" &
kc -n shop scale deployment/payments --replicas=3 >/dev/null
wait
trace="$(trace_of "$(cat "${tmp}/a.json")")"
wait_complete --trace-id "${trace}"
./bin/effecttrace explain --server "${SERVER}" --trace-id "${trace}" | sed -n '1,/^WINDOWS/p'
echo "→ the payments changes appear under EXCLUDED, not in the agent's graph."

step "4. Two agents act on two different workloads at once"
./bin/agent scale_workload namespace=shop deployment=inventory replicas=3 > "${tmp}/b.json" &
./bin/agent restart_workload namespace=shop deployment=frontend > "${tmp}/c.json" &
wait
for f in b c; do
  t="$(trace_of "$(cat "${tmp}/${f}.json")")"
  wait_complete --trace-id "${t}"
  ./bin/effecttrace explain --server "${SERVER}" --trace-id "${t}" | sed -n '1,/^EXCLUDED\|^WINDOWS/p'
done

step "Restoring the shop namespace"
kc apply -f deploy/lab/shop.yaml >/dev/null
echo "done. Temporal correlation does not prove causation."
