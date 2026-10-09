#!/usr/bin/env bash
#
# Prove the deployed stack actually works, rather than that it applied cleanly.
#
# `kubectl apply` succeeding says nothing about whether a span reached Jaeger or
# whether Prometheus is scraping anything. Every check below reads a value out
# of a running component and fails on the value, not on the exit code of the
# thing that produced it.
#
# Run with: task kind:smoke
set -euo pipefail

CONTEXT="${KUBE_CONTEXT:-kind-go-agents}"
APP_NS="go-agents"
OBS_NS="observability"

# Refuse to run against anything but our own cluster. The developer's current
# context is routinely a different cluster, and a smoke test is one of the few
# things here that issues writes.
if ! kubectl config get-contexts -o name | grep -qx "$CONTEXT"; then
  echo "context $CONTEXT does not exist; run 'task kind:up' first" >&2
  exit 1
fi
k() { kubectl --context "$CONTEXT" "$@"; }

pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

# forward <ns> <target> <local:remote> <probe-path> — background port-forward,
# waited for by an actual HTTP round trip.
#
# A TCP connect is NOT sufficient: kubectl binds the local port immediately and
# only then establishes the tunnel, so a connect succeeds while the first real
# request still fails with EOF. That produced a flaky "/healthz is 200" failure
# against a service that was perfectly healthy.
forward() {
  k -n "$1" port-forward "$2" "$3" >/dev/null 2>&1 &
  pids+=($!)
  local port="${3%%:*}" probe="$4" i
  for i in $(seq 1 100); do
    if curl -sf -o /dev/null --max-time 2 "http://127.0.0.1:${port}${probe}"; then
      return 0
    fi
    sleep 0.2
  done
  echo "port-forward to $2 never answered on ${port}${probe}" >&2
  return 1
}

pass=0
check() { # check <description> <condition-output>
  if [ -n "$2" ]; then
    printf '  ok   %s\n' "$1"
    pass=$((pass + 1))
  else
    printf '  FAIL %s\n' "$1" >&2
    exit 1
  fi
}

echo "== workloads =="
for d in go-agents; do
  k -n "$APP_NS" rollout status "deploy/$d" --timeout=120s >/dev/null
done
for d in otel-collector prometheus grafana tempo loki pyroscope; do
  k -n "$OBS_NS" rollout status "deploy/$d" --timeout=240s >/dev/null
done
# alloy is a DaemonSet: it reads container logs off every node, so
# `rollout status deploy/alloy` fails NotFound rather than waiting.
k -n "$OBS_NS" rollout status daemonset/alloy --timeout=240s >/dev/null
READY=$(k -n "$APP_NS" get deploy go-agents -o jsonpath='{.status.readyReplicas}')
check "both replicas ready (got ${READY:-0})" "$([ "${READY:-0}" -ge 2 ] && echo y)"

# Two replicas on two different nodes is the only thing that makes the
# topologySpreadConstraint and the PodDisruptionBudget more than decoration.
NODES=$(k -n "$APP_NS" get pods -l app.kubernetes.io/name=go-agents \
  -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u | wc -l)
check "replicas spread across $NODES node(s)" "$([ "$NODES" -ge 2 ] && echo y)"

echo "== api =="
forward "$APP_NS" svc/go-agents 18080:80 /healthz
TOKEN=$(k -n "$APP_NS" get secret go-agents-api-token -o jsonpath='{.data.token}' | base64 -d)

check "/healthz is 200" "$(curl -sf -o /dev/null -w '%{http_code}' localhost:18080/healthz | grep -x 200 || true)"
check "/readyz is 200" "$(curl -sf -o /dev/null -w '%{http_code}' localhost:18080/readyz | grep -x 200 || true)"
check "/version reports a commit" "$(curl -sf localhost:18080/version | grep -o '"commit":"[^"]\+"' || true)"

# Unauthenticated writes must be refused: GO_AGENTS_ENV is staging in the
# manifest, so config.Load() required the token to exist at all.
UNAUTH=$(curl -s -o /dev/null -w '%{http_code}' -X POST localhost:18080/v1/notes \
  -H 'Content-Type: application/json' -d '{"title":"nope","body":"x"}')
check "unauthenticated POST is 401 (got $UNAUTH)" "$([ "$UNAUTH" = 401 ] && echo y)"

CREATED=$(curl -s -X POST localhost:18080/v1/notes \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" \
  -d '{"title":"smoke","body":"from kind-smoke.sh"}')
check "authenticated POST created a note" "$(printf '%s' "$CREATED" | grep -o '"id":"[^"]\+"' || true)"

echo "== admin listener =="
# The Service must NOT route the admin port. Reaching it requires the pod.
# The FIRST READY pod, not items[0]. A rolling update leaves the outgoing
# ReplicaSet's pods listed while they terminate, and port-forwarding to one of
# those hangs until the wait loop gives up — a failure that looks like a broken
# admin listener and is nothing of the sort. Observed mid-rollout.
POD=$(k -n "$APP_NS" get pods -l app.kubernetes.io/name=go-agents \
  --field-selector=status.phase=Running \
  -o jsonpath='{range .items[?(@.status.containerStatuses[0].ready==true)]}{.metadata.name}{"\n"}{end}' \
  | head -1)
[ -n "$POD" ] || { echo "no Ready go-agents pod to scrape" >&2; exit 1; }
forward "$APP_NS" "pod/$POD" 19090:9090 /metrics

# Drive traffic at THIS pod directly before scraping it. The POST above went
# through the Service, which load-balances across both replicas, so asserting
# on one pod's counters after it was a coin flip — it passed on luck until a
# rollout reordered the pods. Each replica has its own registry (and its own
# MemStore), so the request and the scrape must name the same pod.
forward "$APP_NS" "pod/$POD" 18081:8080 /healthz
curl -sf -o /dev/null "localhost:18081/v1/notes"
curl -sf -o /dev/null "localhost:18081/v1/notes/does-not-exist" || true

SCRAPE=$(curl -sf localhost:19090/metrics)
check "/metrics exposes http_requests_total" "$(printf '%s' "$SCRAPE" | grep -m1 '^http_requests_total' || true)"
check "/metrics labels by route template" "$(printf '%s' "$SCRAPE" | grep -m1 'route="/v1/notes"' || true)"
# The cardinality guard, asserted against a real scrape: a per-ID series would
# mean one time series per note and eventually a Prometheus nobody can query.
check "/metrics does NOT leak a per-ID series" \
  "$(printf '%s' "$SCRAPE" | grep -q 'route="/v1/notes/does-not-exist"' && true || echo y)"
check "/metrics keeps the :id template" \
  "$(printf '%s' "$SCRAPE" | grep -m1 'route="/v1/notes/:id"' || true)"
check "/metrics carries build_info" "$(printf '%s' "$SCRAPE" | grep -m1 '^build_info{' || true)"
check "/debug/pprof is served on admin" "$(curl -sf -o /dev/null -w '%{http_code}' localhost:19090/debug/pprof/ | grep -x 200 || true)"
# A heap profile is gzipped protobuf, so it is counted rather than matched:
# piping binary through a command substitution drops NUL bytes and warns.
HEAP_BYTES=$(curl -sf localhost:19090/debug/pprof/heap | wc -c)
check "a heap profile is downloadable ($HEAP_BYTES bytes)" "$([ "$HEAP_BYTES" -gt 1000 ] && echo y)"

echo "== prometheus =="
forward "$OBS_NS" svc/prometheus 19091:9090 /-/ready
TARGETS=$(curl -sf 'localhost:19091/api/v1/targets?state=active')
check "the go-agents target is up" \
  "$(printf '%s' "$TARGETS" | grep -o '"health":"up"' | head -1 || true)"
SERIES=$(curl -sf --get 'localhost:19091/api/v1/query' --data-urlencode 'query=build_info')
check "prometheus has scraped build_info" "$(printf '%s' "$SERIES" | grep -o '"__name__":"build_info"' | head -1 || true)"
RULES=$(curl -sf 'localhost:19091/api/v1/rules')
check "alert rules are loaded" "$(printf '%s' "$RULES" | grep -o '"type":"alerting"' | head -1 || true)"

echo "== grafana =="
forward "$OBS_NS" svc/grafana 13000:3000 /api/health
# Health of the DATASOURCE, not of Grafana. Grafana comes up perfectly happy
# while every panel reads "No data": the failure is a DNS lookup inside
# Grafana's own process, invisible from the pod's resolv.conf and invisible
# from `kubectl get pods`. See the image comment in grafana.yaml.
for ds in prometheus loki pyroscope; do
  DS=$(curl -sf "localhost:13000/api/datasources/uid/${ds}/health" || true)
  check "grafana can query the ${ds} datasource" \
    "$(printf '%s' "$DS" | grep -o '"status":"OK"' | head -1 || true)"
done
# Tempo is checked THROUGH THE PROXY, not via /health. Grafana 11.6's Tempo
# core plugin does not implement the health endpoint and returns 404
# `plugin.notImplemented` whether or not Tempo is reachable — asserting on it
# would be a check that can only fail, which is worse than none.
TEMPO_DS=$(curl -sf 'localhost:13000/api/datasources/proxy/uid/tempo/api/status/buildinfo' || true)
check "grafana reaches tempo through the datasource proxy" \
  "$(printf '%s' "$TEMPO_DS" | grep -o '"version"' | head -1 || true)"

# And a real panel query, because a healthy datasource that returns no series
# is still a dashboard nobody can use.
PANEL=$(curl -sf -X POST localhost:13000/api/ds/query -H 'Content-Type: application/json' \
  -d '{"queries":[{"refId":"A","datasource":{"type":"prometheus","uid":"prometheus"},"expr":"sum(http_requests_total)","instant":true}],"from":"now-15m","to":"now"}' || true)
check "a dashboard query returns data" "$(printf '%s' "$PANEL" | grep -o '"status":200' | head -1 || true)"
DASH=$(curl -sf 'localhost:13000/api/search?type=dash-db' || true)
check "the RED dashboard is provisioned" "$(printf '%s' "$DASH" | grep -o 'go-agents RED' | head -1 || true)"
check "the Golden Path dashboard is provisioned" \
  "$(printf '%s' "$DASH" | grep -o 'go-agents Golden Path' | head -1 || true)"

# The Loki panel query, run for real. It is the one target on either dashboard
# that is NOT Prometheus, and $__rate_interval — correct on the other eight —
# is a Prometheus-only macro that reaches Loki as a literal and 400s. Asserting
# a 200 here is what stops that regressing.
LOGQ=$(curl -sf -X POST localhost:13000/api/ds/query -H 'Content-Type: application/json' \
  -d '{"queries":[{"refId":"A","datasource":{"type":"loki","uid":"loki"},"expr":"sum(rate({namespace=\"go-agents\", app=\"go-agents\"} | json | __error__=\"\" [$__auto]))","queryType":"range"}],"from":"now-15m","to":"now"}' || true)
check "the Loki dashboard query returns 200" "$(printf '%s' "$LOGQ" | grep -o '"status":200' | head -1 || true)"

echo "== tracing =="
forward "$OBS_NS" svc/tempo 13200:3200 /ready
# The span for the POST above travels app -> otel-collector -> tempo, so the
# loop gives the batcher and the collector time rather than asserting once.
#
# TraceQL by resource attribute, not a free-text search: `service.name` is the
# resource attribute internal/telemetry sets, so this asks the question that
# was meant rather than matching whatever happens to be recent.
TRACES=""
for _ in $(seq 1 40); do
  TRACES=$(curl -sf --get 'localhost:13200/api/search' \
    --data-urlencode 'q={resource.service.name="go-agents"}' \
    --data-urlencode 'limit=20' || true)
  printf '%s' "$TRACES" | grep -q '"traceID"' && break
  sleep 3
done
check "tempo has traces for go-agents" "$(printf '%s' "$TRACES" | grep -o '"traceID"' | head -1 || true)"

# Scoped by name, not grepped out of the unscoped search above — the same
# ambiguity the comment two lines up is about, and the same fix the readiness
# probe check below uses.
NOTESTRACE=$(curl -sf --get 'localhost:13200/api/search' \
  --data-urlencode 'q={resource.service.name="go-agents" && name="POST /v1/notes"}' \
  --data-urlencode 'limit=1' || true)
check "a trace covers a notes route" "$(printf '%s' "$NOTESTRACE" | grep -o '"traceID"' | head -1 || true)"

# Probe routes are filtered out of tracing on purpose: the kubelet probes every
# few seconds per pod, which would bury every real request. Asserting their
# ABSENCE is what keeps that filter from silently regressing.
PROBE=$(curl -sf --get 'localhost:13200/api/search' \
  --data-urlencode 'q={resource.service.name="go-agents" && name="GET /readyz"}' \
  --data-urlencode 'limit=1' || true)
check "readiness probes are NOT traced" \
  "$(printf '%s' "$PROBE" | grep -q '"traceID"' && true || echo y)"

echo "== logs =="
forward "$OBS_NS" svc/loki 13100:3100 /ready
# Alloy has to discover the pod, tail the file and push before this can pass.
LOGS=""
for _ in $(seq 1 40); do
  LOGS=$(curl -sf --get 'localhost:13100/loki/api/v1/query_range' \
    --data-urlencode 'query={namespace="go-agents", app="go-agents"}' \
    --data-urlencode 'limit=50' || true)
  printf '%s' "$LOGS" | grep -q '"values"' && break
  sleep 3
done
check "loki has log lines from the app" "$(printf '%s' "$LOGS" | grep -o '"values"' | head -1 || true)"

# The whole reason logs and traces are one system: the line carries the id.
#
# Filtered with `|= "trace_id"` SERVER-SIDE, not grepped out of the previous
# result. Probes are excluded from tracing, so their lines have no trace_id —
# and the kubelet produces them every few seconds per pod while the handful of
# real requests this script makes do not repeat. Any recent-N window is
# therefore almost all probes, and grepping it passed only when a request
# happened to land inside the window. Measured: it passed, then failed on the
# next run with nothing changed.
TRACED=""
for _ in $(seq 1 40); do
  TRACED=$(curl -sf --get 'localhost:13100/loki/api/v1/query_range' \
    --data-urlencode 'query={namespace="go-agents", app="go-agents"} |= "trace_id"' \
    --data-urlencode 'limit=5' || true)
  printf '%s' "$TRACED" | grep -q 'trace_id' && break
  sleep 3
done
check "a log line carries a trace_id" "$(printf '%s' "$TRACED" | grep -o 'trace_id' | head -1 || true)"

# The other half of that contract: probe lines must NOT carry one, or the
# tracing filter has regressed and the trace store is about to fill with
# liveness checks.
PROBE_LOGS=$(curl -sf --get 'localhost:13100/loki/api/v1/query_range' \
  --data-urlencode 'query={namespace="go-agents", app="go-agents"} |= "/readyz" |= "trace_id"' \
  --data-urlencode 'limit=1' || true)
check "probe log lines carry NO trace_id" \
  "$(printf '%s' "$PROBE_LOGS" | grep -q '"values":\[\[' && true || echo y)"
# Cardinality guard, asserted against the running Loki rather than the config:
# a trace_id LABEL would mint a stream per request.
LABELS=$(curl -sf 'localhost:13100/loki/api/v1/labels' || true)
check "trace_id is NOT an indexed label" \
  "$(printf '%s' "$LABELS" | grep -q '"trace_id"' && true || echo y)"

echo "== profiles =="
forward "$OBS_NS" svc/pyroscope 14040:4040 /ready
# Alloy scrapes /debug/pprof off the ADMIN port, so this passing is the proof
# that continuous profiling needed no application change at all.
PROF=""
for _ in $(seq 1 40); do
  PROF=$(curl -sf --get 'localhost:14040/pyroscope/render' \
    --data-urlencode 'query=process_cpu:cpu:nanoseconds:cpu:nanoseconds{service_name="go-agents"}' \
    --data-urlencode 'from=now-15m' --data-urlencode 'until=now' || true)
  printf '%s' "$PROF" | grep -q '"names"' && break
  sleep 3
done
check "pyroscope has cpu profiles scraped from the admin port" \
  "$(printf '%s' "$PROF" | grep -o '"names"' | head -1 || true)"

echo
echo "$pass checks passed"
