# Incident response

**Use this** when something is wrong and you do not yet know what — a page fired
with no obvious cause, a user reports errors, or a graph moved and nobody
deployed. It is also the `runbook_url` target of `GoAgentsAbsent` and
`GoAgentsHighErrorRate`. **It assumes** nothing about the cause. If you already know which
alert fired, [on-call-triage.md](on-call-triage.md) has a section per alert; if
you already know a deploy did it, go straight to [rollback.md](rollback.md).

## Severity

Pick one in the first minute and say it out loud. Severity here is about blast
radius, not about how alarming the graph looks.

| Sev | What it looks like for this service | Response |
|-----|-------------------------------------|----------|
| **SEV1** | The Service has no ready endpoints, or every request 5xx. Both replicas down, both failing readiness, namespace gone, kind cluster unhealthy. | Drop everything. Rollback is on the table immediately. |
| **SEV2** | One replica down or unready (the other still serves, and the PDB keeps it that way), sustained elevated 5xx or latency on a route, a rollout stalled with old pods still serving. | Work it now, but you have time. Diagnose before you change anything. |
| **SEV3** | Version skew past the rollout window, saturation trending toward the 128Mi limit, the observability stack itself down, a single pod OOMKilled once and recovered. | Fix in hours. Do not restart things at 3am for this. |

`maxUnavailable: 0` and the PodDisruptionBudget (`minAvailable: 1`) mean most
failures here degrade rather than outage. A stalled rollout is a SEV2 by
default, not a SEV1 — the old pods never stopped serving.

## The first five minutes

Run these in order. Each one either escalates or de-escalates the severity you
just picked.

**1. Is anything actually serving?**

```bash
kubectl --context kind-go-agents -n go-agents get pods -l app.kubernetes.io/name=go-agents -o wide
kubectl --context kind-go-agents -n go-agents get endpoints go-agents
```

Expect 2 pods `Running` and `1/1 READY`, on different nodes, and an `ENDPOINTS`
column listing two `IP:8080` pairs.

- Two ready endpoints → not an availability incident. The problem is in
  responses, not in reachability. Continue to step 3.
- One ready endpoint → SEV2. The service is up on one replica. Go to
  [health-check-failure.md](health-check-failure.md) for the unready one.
- `<none>` endpoints → SEV1, total outage. Go to
  [health-check-failure.md](health-check-failure.md) now.
- Pods `Pending` or nodes `NotReady`
  (`kubectl --context kind-go-agents get nodes`) → the cluster, not the app.

**2. Did anything change?**

```bash
kubectl --context kind-go-agents -n go-agents rollout history deployment/go-agents
kubectl --context kind-go-agents -n go-agents get deployment go-agents \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

Expect the newest revision to be one you recognise and `Progressing=True
NewReplicaSetAvailable`. A revision created minutes before the symptom started
is your prime suspect and points at [rollback.md](rollback.md). `build_info` on
the dashboard answers the same question graphically, overlaid on the panel that
moved.

**3. Open the signals** (next section), and note the wall-clock time the symptom
started. Everything after this is correlation, and correlation needs a `t0`.

**4. Pull one failing request end to end** — a log line, its `trace_id`, and the
trace behind it. See [Correlating a log line to a
trace](#correlating-a-log-line-to-a-trace). One real failing request beats ten
minutes of graph-staring.

**5. Decide: mitigate or investigate.** If a deploy correlates, roll back and
investigate afterwards. If nothing changed, keep investigating — a rollback that
reverts nothing costs you both MemStores and your best hypothesis.

## Open the signals

Service names and ports in the `observability` namespace are set by the manifests
in `infrastructure/local/observability`, so **list them rather than guessing**:

```bash
kubectl --context kind-go-agents -n observability get svc
kubectl --context kind-go-agents -n observability get pods
```

Then port-forward the one you want. The upstream default ports, for orientation
only — the `get svc` output above is authoritative:

| Component | Port | Port-forward |
|-----------|------|--------------|
| Grafana | 3000 | `kubectl --context kind-go-agents -n observability port-forward svc/grafana 3000:3000` |
| Prometheus | 9090 | `kubectl --context kind-go-agents -n observability port-forward svc/prometheus 9090:9090` |
| Tempo | 3200 | `kubectl --context kind-go-agents -n observability port-forward svc/tempo 3200:3200` |
| Loki | 3100 | `kubectl --context kind-go-agents -n observability port-forward svc/loki 3100:3100` |
| Pyroscope | 4040 | `kubectl --context kind-go-agents -n observability port-forward svc/pyroscope 4040:4040` |
| OTLP collector (HTTP) | 4318 | `svc/otel-collector`; not usually needed by hand |

`task k8s:ui` starts all of them at once and prints the URLs, which is what you
want at 3am rather than six terminals.

**Tempo, Loki and Pyroscope have no UI of their own** — they are APIs. Reach
them through Grafana's Explore, which is why all four are provisioned as
datasources and why the correlations between them are configured.

Then open `http://localhost:<port>` in a browser. **Grafana asks for no
credentials** — anonymous access is Admin and the login form is disabled. What
bounds that is that there is no Ingress and no NodePort, so nothing outside the
cluster can reach it. Port-forward is how a human gets in; it is not what keeps
anything else out. Inside the cluster Grafana is open to any pod, because no
NetworkPolicy exists here. The upside is that there is no password in this
repository to leak. The provisioned dashboard is
**"go-agents RED"** (uid `go-agents-red`): request rate by route, 5xx
ratio, p50/p95/p99 latency, in-flight requests, and a deployed-build panel.
Start there; drop to the Prometheus expression browser when you need a query
the dashboard does not have.

Four queries worth having ready in Prometheus:

```promql
# Are the app targets being scraped at all? Expect two series, both 1.
up{job="go-agents"}

# Error rate, as a fraction of requests, by route.
sum by (route) (rate(http_requests_total{job="go-agents",status=~"5.."}[5m]))
  / sum by (route) (rate(http_requests_total{job="go-agents"}[5m]))

# p99 latency by route — the quantile the latency alert uses.
histogram_quantile(0.99,
  sum by (route, le) (rate(http_request_duration_seconds_bucket{job="go-agents"}[5m])))

# What is deployed, per replica.
count by (version, commit, env) (build_info{job="go-agents"})
```

The `job="go-agents"` label is produced by a relabel in the Prometheus scrape
config, not by the pod. If that relabel breaks, these queries and **every alert
rule** return nothing, and the failure looks exactly like health. See
[on-call-triage.md](on-call-triage.md#before-you-trust-any-of-these).

If Prometheus itself is unreachable, you have lost your view but not your
service. Fall back to `kubectl logs` and the per-pod `/metrics` scrape described
under [Reading one pod's metrics
directly](#reading-one-pods-metrics-directly). Do not treat a broken
observability stack as an application incident.

## Correlating a log line to a trace

Every request log line is JSON and carries `trace_id` and `span_id`, because the
tracing middleware runs *before* the request logger and the logger reads the
span off the context. The sample ratio is `1.0` in this deployment, so **every
non-probe request has a trace in Tempo** — a local-cluster choice, and under a
production sample ratio this technique would only find sampled requests.

`/healthz` and `/readyz` are deliberately excluded from tracing: the kubelet
hits them every 10s and 5s per pod forever, and tracing that would bury every
real request in the trace store. So a probe log line carries no usable
`trace_id`, and "no trace for this request" on a probe is correct behaviour
rather than a dropped span.

Which pod produced a line matters here (the two replicas hold different data),
and `kubectl logs --prefix` breaks `jq` by prepending a pod tag. So iterate pods:

```bash
for p in $(kubectl --context kind-go-agents -n go-agents \
    get pods -l app.kubernetes.io/name=go-agents -o name); do
  echo "== $p"
  kubectl --context kind-go-agents -n go-agents logs "$p" --tail=500 \
    | jq -rc 'select(.status >= 500) | {time, status, method, path, trace_id}'
done
```

Expect lines like
`{"time":"…","status":500,"method":"POST","path":"/v1/notes","trace_id":"4bf92f…"}`.

You do not have to copy it by hand. In Grafana, open the log line in Explore
(Loki) and click **TraceID** on the row — the Loki datasource carries a derived
field that regexes `trace_id` out of the raw line and links straight to the
trace in Tempo. Going the other way, a span in Tempo has a **Logs** button
(`tracesToLogsV2`) that filters Loki to that namespace/app and trace, and a
**Metrics** button that lands on the RED series for the span's route.

The manual paths still work when Grafana is the thing that is broken:

```bash
# trace id -> trace
curl -sf --get localhost:3200/api/traces/<trace id> | jq .

# trace id -> log lines, straight from the pods
# (not `task k8s:logs`: it hardcodes --prefix, which prepends a pod tag that
# breaks jq the same way described above)
kubectl --context kind-go-agents -n go-agents logs -l app.kubernetes.io/name=go-agents \
    --all-containers --tail=2000 | jq -rc 'select(.trace_id == "<trace id>")'
```

Three things to know before you distrust what you read:

- Log output passes through a scrubbing writer, so a redacted-looking value in a
  field is redaction working, not corruption.
- `kubectl logs` shows only the current container. For a pod that restarted, add
  `--previous` to see what it said on the way down.
- **Tempo and Loki both store to an emptyDir**, and Tempo additionally expires
  blocks (`block_retention: 6h`). A `trace_id` from an old log line may be gone,
  and deleting either pod discards everything it holds. Pull what you need early
  in an incident, not at the write-up. This is the honest local trade: real
  components, disposable storage.

## Reading one pod's metrics directly

The admin port is not in the Service, so this needs a **named pod**. With two
replicas, "which pod" is always a real question here.

```bash
kubectl --context kind-go-agents -n go-agents get pods -l app.kubernetes.io/name=go-agents
POD=pod/<paste the one you want>

kubectl --context kind-go-agents -n go-agents port-forward "$POD" 9090:9090
# in another shell:
curl -s localhost:9090/metrics | grep -E '^(build_info|http_requests_in_flight|http_requests_total)'
```

Do **not** use `port-forward deployment/go-agents`: it silently picks one
arbitrary pod, and you will not know which.

## Pulling a pprof profile

Same port-forward, same named pod. `/debug/pprof/` serves the named profiles
even though only `cmdline`, `profile`, `symbol` and `trace` are registered
explicitly — the index handler dispatches the rest.

```bash
kubectl --context kind-go-agents -n go-agents port-forward pod/<name> 9090:9090
```

Then, in another shell:

```bash
# Heap — for suspected leaks, or a pod walking toward the 128Mi limit.
go tool pprof -http=: http://localhost:9090/debug/pprof/heap

# Goroutines — for a wedged process, or a count that only ever grows.
curl -s http://localhost:9090/debug/pprof/goroutine?debug=1 | head -40

# CPU, 30 seconds. Take this while the load that concerns you is happening.
curl -s -o /tmp/cpu.pprof 'http://localhost:9090/debug/pprof/profile?seconds=30'
go tool pprof -http=: /tmp/cpu.pprof
```

The CPU profile blocks for the full 30s before writing a byte. That works
because the admin server's `WriteTimeout` is deliberately zero; a write deadline
shorter than `seconds` would truncate every profile. Quote the URL — an
unquoted `?seconds=30` is a shell glob.

Write the profile to your machine, not into the container: the root filesystem
is read-only, so there is nowhere writable to put it. (The runtime image is
Debian-based and does have a shell — see `task k8s:shell` — but that does not
help here; the rootfs itself cannot be written to.)

## Not an incident

Three symptoms that look like bugs and are not. Confirm you are not chasing one
of these before you escalate.

- **A `404` on a note that was just created successfully.** The two replicas do
  not share state, and the Service load-balances. The note exists in the other
  pod.
- **Every note gone after a restart, rollout or OOMKill.** `MemStore` is
  in-process. There is no persistence to have lost.
- **Two `build_info` commits during a rollout.** `maxSurge: 1` means old and new
  run together by design. It is only a problem past
  `progressDeadlineSeconds` (120s) — see [version
  skew](on-call-triage.md#version-skew).

There is also no outbound dependency in the request path. If the API is
misbehaving, the cause is in this process, its configuration, or the cluster
under it — there is no upstream to blame and no circuit breaker to check.

## Comms and escalation

There is no pager rotation for this service; it is a single service in a local
cluster. What follows is the shape to keep even so, because the shape is what
makes a handover possible.

1. **Declare.** One message, one channel, at the top: severity, one sentence of
   symptom, when it started, who is driving. "SEV2, ~40% 5xx on
   `POST /v1/notes` since 14:05, I'm on it."
2. **One driver.** One person runs commands. Everyone else investigates and
   reports. Two people running `rollout undo` is its own incident.
3. **Update on a clock**, not on progress: every 15 minutes for a SEV1, every 30
   for a SEV2, even when the update is "still looking, no new information".
   Silence reads as either "fixed" or "abandoned", and it is never either.
4. **Escalate on time, not on despair.** No credible hypothesis after 30 minutes
   on a SEV1, or a mitigation that failed, means bring in a second person. State
   what you have ruled out, not what you have tried.
5. **Hand over with five facts**: symptom, `t0`, what changed (or that nothing
   did), what you have ruled out, what you were about to do next.
6. **Close explicitly**, and record the `t0`, the trace IDs and the commit
   involved. Those three are what a postmortem cannot reconstruct afterwards —
   the pod logs are gone as soon as the pod is.
