#!/usr/bin/env bash
#
# Port-forward every UI in the local cluster at once and print where they are.
#
# Six separate `kubectl port-forward` invocations is what this replaces, and the
# reason it is a script rather than six tasks is that they have to run
# CONCURRENTLY and be torn down together — six tasks means six terminals and six
# orphaned forwards when you close them.
#
# Run with: task k8s:ui        (or: task k8s:ui -- grafana tempo)
set -uo pipefail

CONTEXT="${KUBE_CONTEXT:-kind-go-agents}"

# name|namespace|service|local:remote|path
TARGETS=(
  "app|go-agents|svc/go-agents|8080:80|/healthz"
  "grafana|observability|svc/grafana|3000:3000|/api/health"
  "prometheus|observability|svc/prometheus|9090:9090|/-/ready"
  "tempo|observability|svc/tempo|3200:3200|/ready"
  "loki|observability|svc/loki|3100:3100|/ready"
  "pyroscope|observability|svc/pyroscope|4040:4040|/ready"
)

if ! kubectl config get-contexts -o name | grep -qx "$CONTEXT"; then
  echo "context $CONTEXT does not exist — run 'task kind:up' first" >&2
  exit 1
fi

# Optional filter: `task k8s:ui -- grafana tempo` forwards only those.
WANT=("$@")
wanted() {
  [ ${#WANT[@]} -eq 0 ] && return 0
  local w
  for w in "${WANT[@]}"; do [ "$w" = "$1" ] && return 0; done
  return 1
}

pids=()
statedir=$(mktemp -d)
cleanup() {
  printf '\nstopping %d forward(s)\n' "${#pids[@]}"
  for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done
  rm -rf "$statedir"
}
trap cleanup EXIT INT TERM

started=()
for t in "${TARGETS[@]}"; do
  IFS='|' read -r name ns svc ports path <<<"$t"
  wanted "$name" || continue
  # A component that is not deployed yet is skipped, not fatal: the stack is
  # applied in pieces and a missing Loki should not stop you reaching Grafana.
  if ! kubectl --context "$CONTEXT" -n "$ns" get "$svc" >/dev/null 2>&1; then
    printf '  %-11s not deployed, skipping\n' "$name"
    continue
  fi
  kubectl --context "$CONTEXT" -n "$ns" port-forward "$svc" "${ports}" >/dev/null 2>&1 &
  pids+=($!)
  started+=("$name|${ports%%:*}|$path")
done

if [ ${#started[@]} -eq 0 ]; then
  echo "nothing to forward" >&2
  exit 1
fi

printf '\n  %-11s %-30s %s\n' "COMPONENT" "URL" "READY"
# Poll every service's readiness concurrently, the same way the port-forwards
# above start concurrently: run one at a time, a slow service near the front
# of TARGETS adds its own up-to-30s wait in front of every service after it.
# Backgrounded and joined with `wait`, the whole table is ready in as long as
# the SLOWEST service takes, not the sum of all of them.
poll_pids=()
for s in "${started[@]}"; do
  IFS='|' read -r name port path <<<"$s"
  (
    # Wait on a real HTTP answer, not a TCP connect: kubectl binds the local
    # port before the tunnel exists, so a connect succeeds while the first
    # request still fails.
    state="starting"
    for _ in $(seq 1 60); do
      if curl -sf -o /dev/null --max-time 2 "http://127.0.0.1:${port}${path}"; then
        state="ok"; break
      fi
      sleep 0.5
    done
    printf '%s' "$state" >"$statedir/$name"
  ) &
  poll_pids+=($!)
done
wait "${poll_pids[@]}"
for s in "${started[@]}"; do
  IFS='|' read -r name port path <<<"$s"
  state=$(cat "$statedir/$name" 2>/dev/null || echo starting)
  printf '  %-11s %-30s %s\n' "$name" "http://localhost:${port}" "$state"
done

cat <<'EOF'

  grafana     -> dashboards "go-agents RED" and "go-agents Golden Path"
  prometheus  -> /alerts for rule state, /targets for scrape health
  pyroscope   -> continuous profiles scraped from the admin port

  Ctrl-C to stop all forwards.
EOF

# Wait on the forwards rather than exiting; the trap tears them down.
wait
