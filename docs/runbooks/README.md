# Runbooks

Start here when a page fires, a deploy misbehaves, or something looks wrong and
you do not yet know what. Every runbook below assumes the service is deployed to
the local kind cluster from `infrastructure/local/k8s`, and that `kubectl` (v1.35+), `docker`,
`kind` and `jq` are on your path.

## Always pass `--context kind-go-agents`

Every command in these runbooks carries `--context kind-go-agents`
explicitly, and yours must too. The cluster this service runs in is *not* the
context a developer here is usually sitting in — `kubectl config current-context`
commonly reads `kind-sdp`. A runbook that relies on the current context is a
runbook that will one day roll back the wrong Deployment in the wrong cluster,
under time pressure, while everyone is watching. The flag costs 30 characters.
It is stated once here and silently carried everywhere else.

The kind cluster is named `go-agents` (so `kind` subcommands take
`--name go-agents`), the app namespace is `go-agents`, and the
observability stack lives in namespace `observability`.

## The service, in one paragraph

`go-agents` is a Go REST API over an in-process notes domain, deployed as a
2-replica Deployment in namespace `go-agents` from the image
`go-agents:<short-sha>` (with a `-dirty-<HHMMSS>` suffix when it was built from an uncommitted tree) — every build is tagged from its commit, which is
what makes `rollout undo` a real rollback. The `go-agents:dev` in
`infrastructure/local/k8s` is a placeholder, substituted for that tag before apply and never
run. It binds **two** ports, and which routes live on which is a security
boundary rather than a layout choice. Port `8080` (named `http`) is
published through the ClusterIP Service on port 80 and serves `/healthz`,
`/readyz`, `/version` and `/v1/notes[/:id]`. Port `9090` (named `admin`)
serves `/metrics` and `/debug/pprof/*` and is deliberately **absent** from the
Service — Prometheus reaches it by scraping the pod IP directly, driven by the
`prometheus.io/scrape`, `prometheus.io/port` and `prometheus.io/path` pod
annotations, and nothing else in the cluster can route to it. Reads are
unauthenticated; `POST` and `DELETE` require a bearer token held in the
`go-agents-api-token` Secret. Spans go over OTLP/HTTP to
`otel-collector.observability.svc.cluster.local:4318` at a sample ratio of 1.0,
and every non-probe request log line is JSON carrying `trace_id` and `span_id`,
so a log line and a trace can be joined. `/healthz` and `/readyz` are excluded
from tracing — see
[incident-response.md](incident-response.md#correlating-a-log-line-to-a-trace) —
so their log lines carry neither.

## Where the signals live

| Signal | Where | How to reach it |
|--------|-------|-----------------|
| Dashboard | Grafana, ns `observability`, dashboards "go-agents RED" and "go-agents Golden Path" | port-forward — see [incident-response.md](incident-response.md#open-the-signals) |
| Metrics | Prometheus, ns `observability` | same |
| Traces | Tempo, ns `observability` | Grafana Explore, or `task k8s:ui -- tempo` |
| Logs (indexed) | Loki, ns `observability` | Grafana Explore, or `task k8s:ui -- loki` |
| Profiles (continuous) | Pyroscope, ns `observability` | Grafana, or `task k8s:ui -- pyroscope` |
| Alert rules | `infrastructure/local/observability/alerts/rules.yaml` | in this repo |
| Logs (raw) | stdout, JSON | `task k8s:logs` |
| Profiles (one-off) | admin port 9090, per pod | `task k8s:profile -- heap` — forwards a **named pod**, not the Service |

The metrics the service actually exports are `http_requests_total`,
`http_request_duration_seconds`, `http_requests_in_flight` and `build_info`,
plus the standard `go_*` and `process_*` collectors, `promhttp_metric_handler_errors_total`
(from the `/metrics` handler itself), and Prometheus's own `up`.
`route` is always the gin route *template* (`/v1/notes/:id`), never the raw
path, or the literal `unmatched` — so a note ID never mints a new time series.

## The MemStore caveat — read this before you diagnose anything

Storage is `notes.MemStore`, an in-process map inside the server binary. There
is no database, no volume and no shared cache. Three consequences an on-call
engineer will meet head-on:

1. **The two replicas do not share state.** A note created through the Service
   lands in exactly one pod. The next `GET` may be balanced to the other pod and
   return `404`. This looks precisely like a data-loss bug and is not one.
2. **Any pod restart wipes that pod's notes.** A rollout, an OOMKill, a node
   drain, a `rollout restart` — all of them are total data loss for that
   replica, by design.
3. **Scaling up does not spread load over existing data.** A new pod starts
   empty. There is no autoscaler here, so this only happens when someone runs
   `kubectl scale` by hand.

Do not open an incident for a `404` after a successful `POST`, and do not spend
the first five minutes hunting a persistence bug. There is also no outbound
dependency in the request path, so there is no circuit breaker, no retry budget
and no upstream to blame: if the API is returning errors, the cause is in this
process, its config, or the cluster underneath it.

## Index

The `runbook_url` on each alert in `infrastructure/local/observability/alerts/rules.yaml`
points at one of these files. This table is that routing, read back:

| Scenario | Runbook | Alert whose `runbook_url` points here |
|----------|---------|---------------------------------------|
| Ship a new build to the cluster | [deploy.md](deploy.md) | none — planned work |
| A deploy made things worse; get back to the last good state | [rollback.md](rollback.md) | `GoAgentsVersionSkew` |
| The service is erroring, or an instance stopped answering | [incident-response.md](incident-response.md) | `GoAgentsAbsent`, `GoAgentsHighErrorRate` |
| Pods restarting, never becoming ready, or dropping out of the Service | [health-check-failure.md](health-check-failure.md) | `GoAgentsReadinessFailing` |
| Any alert, with a section per alert | [on-call-triage.md](on-call-triage.md) | `GoAgentsNoTargets`, `GoAgentsHighLatency`, `GoAgentsSaturation` |

[incident-response.md](incident-response.md) is also the entry point when
nothing has paged and you do not yet know what is wrong.

[on-call-triage.md](on-call-triage.md) carries a section for **every** alert in
the rule file, not only the three routed to it, and is organised by *symptom*
with the alert name as a field — so a renamed alert costs you one lookup rather
than a wrong turn. The rule file is the source of truth for names, thresholds
and durations.
