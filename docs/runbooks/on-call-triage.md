# On-call triage

**Use this** when a named alert has fired and you want the section for it. It is
the `runbook_url` target of `GoAgentsNoTargets`, `GoAgentsHighLatency` and
`GoAgentsSaturation`, and carries a section for every other alert too.
**It assumes** you have already run the first two steps of
[incident-response.md](incident-response.md#the-first-five-minutes) — pod state
and recent rollouts — because every section below asks you to have them.

There is one section per alert in `infrastructure/local/observability/alerts/rules.yaml`,
keyed by **symptom** with the alert name as a field, so a renamed alert costs
you one lookup rather than a wrong turn. Thresholds and `for:` durations are
quoted here to save you a file open, but **the rule file is the source of
truth** — if a number here disagrees with it, the rule wins and this document is
stale.

Reach Prometheus and the dashboard via the port-forwards in
[incident-response.md](incident-response.md#open-the-signals).

## Before you trust any of these

Every expression in the rule file selects `job="go-agents"`. Discovery is
annotation-based, so targets arrive labelled with the scrape config's own
`job_name` — `kubernetes-pods` — and `infrastructure/local/observability/prometheus.yaml`
relabels `__meta_kubernetes_pod_label_app_kubernetes_io_name` onto `job` to fix
that. **If that relabel is removed, every rule matches nothing and no alert ever
fires.** The failure mode is silence, not an error. Confirm the label exists
before concluding that quiet means healthy:

```promql
up{job="go-agents"}
```

Two series, both `1`, is healthy. *No* series at all is either a real outage or
a broken relabel, and `GoAgentsNoTargets` is the only rule that can tell the
difference.

---

## No targets

**Alert:** `GoAgentsNoTargets` — critical, `for: 5m`
**Expression:** `absent(up{job="go-agents"})`

### What it means

Prometheus is discovering **zero** targets for this service. This is not the
same as "the targets are down": `up == 0` cannot fire when the `up` series does
not exist, so in this state every other rule in the file — errors, latency,
saturation, readiness — reads as clean. `GoAgentsNoTargets` exists precisely
because nothing else notices.

Its firing tells you nothing yet about whether users are affected. Find out
first.

### Check first

```bash
kubectl --context kind-go-agents -n go-agents get pods -l app.kubernetes.io/name=go-agents -o wide
kubectl --context kind-go-agents -n go-agents get endpoints go-agents
```

- **Pods running, endpoints present** — users are fine. This is a monitoring
  break, SEV3. Go to the relabel/annotation causes below.
- **No pods, or no namespace** — the Deployment is gone. SEV1, and the question
  is who or what deleted it.

Then read the scrape error verbatim from Prometheus's own Targets page, which
usually names the cause outright.

### Likely causes

- **The `job` relabel was dropped or changed** in the Prometheus scrape config.
  The pods are healthy and invisible.
- **The pod annotations were removed** — `prometheus.io/scrape: "true"`,
  `prometheus.io/port: "9090"`, `prometheus.io/path: /metrics` on the pod
  template. Nothing else in the repo fails when these are dropped.
- **The Deployment or namespace was deleted.**
- **Prometheus lost its RBAC** to list pods, so discovery returns nothing.

### Action

If pods are serving, fix the observability path and do not touch the
application. If the pods are gone, this is an outage: redeploy per
[deploy.md](deploy.md). Never restart the app to clear a discovery alert.

---

## Instance not scrapeable

**Alert:** `GoAgentsAbsent` — critical, `for: 2m`
**Expression:** `up{job="go-agents"} == 0`

### What it means

A pod is discovered but its admin listener on `:9090` is not answering. The
`for: 2m` is four consecutive rule *evaluations* at the group's 30s `interval` —
not four scrapes; those two minutes span roughly eight of them at the 15s
`scrape_interval`. Because a pod being replaced during a rolling update
*disappears* from discovery rather than scraping as `0`, this does not fire on a
healthy rollout. It points at a dead or wedged process.

It is a statement about the admin listener, not the API listener. They are
separate `http.Server` values in the same process, so one can be down while the
other serves. Do not assume an outage; check.

### Check first

```bash
kubectl --context kind-go-agents -n go-agents get pods -l app.kubernetes.io/name=go-agents -o wide
kubectl --context kind-go-agents -n go-agents get endpoints go-agents
kubectl --context kind-go-agents -n go-agents describe pod <name> | tail -30
```

Two ready endpoints means users are still being served on the other replica.
No endpoints means SEV1.

Then ask the pod yourself, over a port-forward to that **named** pod:

```bash
kubectl --context kind-go-agents -n go-agents port-forward pod/<name> 9090:9090
curl -s -o /dev/null -w '%{http_code}\n' localhost:9090/metrics
```

### Likely causes

- **The pod is restarting or gone** — the scrape failure is a symptom.
  [health-check-failure.md](health-check-failure.md) is the real runbook.
- **The process is wedged.** The admin listener normally survives a busy API
  listener, so a wedged admin port means the whole process, not just load.
  Profile before restarting, per the action below.
- **OOMKilled**, mid-restart. `lastState` will say so.
- **Network policy or a changed containerPort** — rare, and only after a
  manifest edit.

### Action

Take a goroutine profile *before* anything restarts, if the port answers at all;
it is the only evidence of a wedge and it dies with the pod. Then work
[health-check-failure.md](health-check-failure.md). If both instances are absent
and the Service has no endpoints, escalate to SEV1 and consider
[rollback.md](rollback.md) if a deploy preceded it.

---

## High error rate

**Alert:** `GoAgentsHighErrorRate` — critical, `for: 5m`
**Expression shape:** the 5xx share of non-probe requests over 5m, **> 0.05**,
gated on **> 0.5 req/s** of non-probe traffic.

```promql
sum by (job) (rate(http_requests_total{job="go-agents",route!~"/healthz|/readyz",status=~"5.."}[5m]))
  / sum by (job) (rate(http_requests_total{job="go-agents",route!~"/healthz|/readyz"}[5m]))
```

### What it means

More than 5% of real requests are 5xx. Three properties of the rule change how
you read it:

- **Probe routes are excluded.** The kubelet hits `/readyz` every 5s and
  `/healthz` every 10s per pod, so on an idle service probe traffic is nearly
  all the traffic. Including it would dilute a real error ratio into
  invisibility.
- **There is a 0.5 req/s floor.** One 500 out of two requests is a 50% error
  rate and is not an incident. The corollary is worth knowing at 3am: **on an
  idle service this alert cannot fire at all**, however broken the code is.
  Absence of this page is not evidence of health on a quiet cluster.
- **4xx is not this alert.** The domain's `ErrNotFound` and `ErrInvalid` map to
  404 and 400 by design. A flood of 401s is a client with the wrong token and a
  flood of 400s is a client sending bad JSON — worth noticing, not this page.

Every 5xx from this API is `{"error":"internal error"}` from `Server.fail`, for
an error it did not recognise. So a 5xx means something the code did not
anticipate, and it was logged.

### Check first

Split by route and status, because the two shapes mean different things:

```promql
sum by (route, status) (rate(http_requests_total{job="go-agents"}[5m]))
```

- **One route** — the fault is in that handler's path.
- **Every route** — the process or the cluster, not one code path. Go to
  [incident-response.md](incident-response.md).

Then read an actual failing request. The 500 branch logs `unhandled error` with
the real error, which is never echoed to the client, so the log is the **only**
place the cause exists:

```bash
kubectl --context kind-go-agents -n go-agents logs \
  -l app.kubernetes.io/name=go-agents --tail=500 \
  | jq -rc 'select(.message == "unhandled error")'
```

Take the `trace_id` from that line into Tempo (or just click TraceID on the Loki row in Grafana) —
[correlating a log line to a trace](incident-response.md#correlating-a-log-line-to-a-trace).

### Likely causes

- **A deploy.** Check `build_info` against the time the ratio moved. By far the
  most common cause and the fastest to disprove.
- **A panic recovered by `gin.Recovery`**, which produces a 500 and a stack in
  the logs.
- **Request-context cancellation under load.** Every store method turns a
  cancelled context into an error, and `fail` maps it to 500 because it is not a
  domain sentinel. Correlate with [saturation](#saturation) and
  [high latency](#high-latency).

There is no upstream dependency in the request path, so "a downstream service is
erroring" is not available as an explanation. The cause is in this process.

### Action

If a deploy correlates, [rollback.md](rollback.md). Otherwise capture a
`trace_id` and an `unhandled error` log line **before** anything restarts: pod
logs vanish with the pod, and a restart is the fastest way to destroy the only
record of the cause.

---

## High latency

**Alert:** `GoAgentsHighLatency` — warning, `for: 10m`
**Expression shape:** p99 **per route**, **> 0.5s**, gated on **> 0.2 req/s** on
that route, probes excluded.

```promql
histogram_quantile(0.99,
  sum by (job, route, le) (rate(http_request_duration_seconds_bucket{job="go-agents",route!~"/healthz|/readyz"}[5m])))
```

### What it means

A named route's p99 is above 500ms. Calibrate before reacting: this service
answers from an in-memory map, and the histogram buckets top out at 4s precisely
because normal responses land in the first few (1ms–25ms). A p99 of 500ms here
is not "a bit slow", it is two orders of magnitude off.

It is a warning with a 10m `for` on top of a 5m rate window, so by the time it
pages the excursion has survived a GC pause, a node hiccup and a scrape gap. It
is real. It is also not, by itself, an outage.

`$labels.route` in the alert names the slow endpoint. Start there.

### Check first

```promql
# The alerting quantile, by route.
histogram_quantile(0.99, sum by (route, le) (rate(http_request_duration_seconds_bucket{job="go-agents"}[5m])))

# Is it everything, or just the tail? Compare p50 and p95 against it.
histogram_quantile(0.50, sum by (le) (rate(http_request_duration_seconds_bucket{job="go-agents"}[5m])))
histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{job="go-agents"}[5m])))

# Is anything queued?
http_requests_in_flight{job="go-agents"}
```

- **p50 moved with p99** — everything is slow. Process-wide: CPU starvation, GC
  pressure, lock contention.
- **p99 only** — a tail. Look for GC pauses (`go_gc_duration_seconds`).
- **`/v1/notes` (list) specifically** — `List` copies and sorts every note
  under a read lock on each call, so its latency grows with the number of notes
  that replica holds. A client that has inserted a very large number of notes
  shows up here first, and on one replica only.

Rising in-flight *with* rising latency is queueing, which is
[saturation](#saturation). Flat in-flight with rising latency is the work itself
getting slower.

### Likely causes

- **A deploy** — `build_info` overlay again.
- **CPU starvation on the node.** There is deliberately **no CPU limit** on this
  container (a limit would throttle latency-sensitive handling for no isolation
  benefit), so cgroup throttling is not the cause — but a busy kind node still
  is.
- **GC pressure approaching the 128Mi memory limit.**
  `go_memstats_heap_inuse_bytes` and `go_gc_duration_seconds`.
- **Lock contention** on the store mutex under concurrent writes. A goroutine
  profile shows it: many goroutines parked in `sync.RWMutex`.

### Action

Take a 30s CPU profile from the slow pod **while it is slow** —
[pulling a pprof profile](incident-response.md#pulling-a-pprof-profile). It is
the one signal that cannot be reconstructed afterwards, and the admin listener's
`WriteTimeout` is zero specifically so the profile completes. Then decide on
rollback.

---

## Saturation

**Alert:** `GoAgentsSaturation` — warning, `for: 5m`
**Expression:** `avg_over_time(http_requests_in_flight{job="go-agents"}[5m]) > 20`

### What it means

A pod averaged more than 20 concurrent in-flight requests over five minutes.
The threshold is anchored to the pod, not to Go: the container requests **50m
CPU**, a twentieth of a core. Twenty requests resident on that budget are
queueing behind each other, not being served in parallel.

`avg_over_time` rather than a bare gauge with a long `for` is deliberate — `for`
demands the condition hold at *every* evaluation, so one sample dipping under
would reset the timer and a genuinely saturated service would never page.

**This alert is about concurrency only.** Memory saturation has no rule: nothing
watches resident memory against the 128Mi limit, so the first signal of a memory
problem is the OOMKill itself (exit 137, `reason: OOMKilled` in `lastState`), by
which time that replica's entire MemStore is gone. Check memory by hand
whenever you are here.

### Check first

```promql
http_requests_in_flight{job="go-agents"}
go_memstats_heap_inuse_bytes{job="go-agents"}
process_resident_memory_bytes{job="go-agents"}
go_goroutines{job="go-agents"}
```

```bash
kubectl --context kind-go-agents -n go-agents get pod <name> \
  -o jsonpath='{.status.containerStatuses[0].lastState}{"\n"}'
```

`OOMKilled` there means it has already happened at least once.

Which pod matters. With `MemStore`, one replica can be carrying far more notes
than the other, and only that one saturates. The alert labels carry `instance`.

### Likely causes

- **Real load** exceeding what a 50m-CPU-request pod handles. There is **no
  autoscaler**, so nothing will react on its own.
- **Notes accumulating in memory.** `MemStore` never evicts and nothing expires.
  Bodies are capped at 4096 bytes and titles at 200, so this needs volume rather
  than one large request — and larger maps make `List` slower, which feeds back
  into in-flight.
- **A goroutine leak** — `go_goroutines` climbing monotonically.
- **A slow route holding requests open**, which is [high latency](#high-latency)
  seen from the other side.

### Action

1. **Take a heap profile before restarting anything** — the pod is the evidence:
   `go tool pprof -http=: http://localhost:9090/debug/pprof/heap` over a
   port-forward to that named pod.
2. If it is note accumulation on a demo cluster, deleting the pod clears it, and
   clears the data with it. That is the caveat, not a bug.
3. Scaling up (`kubectl --context kind-go-agents -n go-agents scale
   deployment/go-agents --replicas=3`) adds capacity, but the new pod starts
   with an **empty store** and will 404 every existing note. Understand that
   before you type it.
4. Raising `resources.limits.memory` or the CPU request is a manifest change and
   therefore a deploy, not a mitigation available mid-incident.

---

## Readiness failing

**Alert:** `GoAgentsReadinessFailing` — critical, `for: 5m`
**Expression shape:** the 503 share of `/readyz` responses over 5m, **> 0.5**,
per instance.

```promql
sum by (job, instance) (rate(http_requests_total{job="go-agents",route="/readyz",status="503"}[5m]))
  / sum by (job, instance) (rate(http_requests_total{job="go-agents",route="/readyz"}[5m]))
```

### What it means

More than half of a pod's readiness probes returned 503 for five minutes. The
readiness probe removes a pod from the Service on the second consecutive
failure, so those 503s never reach a user and never appear in
`GoAgentsHighErrorRate` — without this rule the symptom would be capacity
quietly halving with no alert attached.

It is expressed as a ratio rather than a rate on purpose: a rate threshold would
hard-code the kubelet's 5s `periodSeconds` and would silently stop working the
day the probe config changes.

**Read this before you go looking for a broken dependency.** `/readyz` calls
`store.List`, the store is `notes.MemStore`, and `MemStore.List` returns an
error only when the *request context is already cancelled*. It takes a read
lock, copies a map and sorts it; nothing else in it can fail. **There is no
backing store here that can be down.** In practice this alert fires when the
process is too slow to answer `/readyz` inside the 2s probe timeout — which is
saturation — and it will *not* fire for the most common readiness failures at
all, because a pod that is crashed, wedged or not listening produces no
`/readyz` samples to be 503. Those show up as `GoAgentsAbsent` instead.

So: **this alert firing means "slow", and readiness failures you can see in
`kubectl` with this alert silent are the normal case.** Trust the pod state over
the alert.

### Check first

```bash
kubectl --context kind-go-agents -n go-agents get pods -l app.kubernetes.io/name=go-agents
kubectl --context kind-go-agents -n go-agents get endpoints go-agents
kubectl --context kind-go-agents -n go-agents describe pod <name> | tail -30
```

The Events tail names the probe and the failure verbatim. The readiness budget
is 5s × 2 failures ≈ 10s, so a pod has been unreachable for at least that long
before it leaves the Service.

Then check saturation on that instance — `http_requests_in_flight` and
`go_memstats_heap_inuse_bytes` — because that is the mechanism that makes this
particular alert possible.

### Likely causes

- **The process is saturated** and cannot answer inside the 2s probe timeout.
  This is the cause this alert is actually capable of catching.
- **GC pauses** long enough to blow the 2s timeout, which points back at memory.

And the causes that will *not* produce this alert, but will produce an unready
pod:

- **Still starting.** The startup probe gets 30s (2s × 15) before liveness and
  readiness begin at all.
- **`CrashLoopBackOff`**, most often the missing `go-agents-api-token`
  Secret, since `GO_AGENTS_ENV=staging` makes it mandatory.
- **`Terminating`** during a rollout or drain. Unready is correct there;
  `terminationGracePeriodSeconds` is 30 against a 10s drain.

### Action

Work [health-check-failure.md](health-check-failure.md), which is this alert's
full procedure and covers the silent cases too. If **both** pods are unready,
escalate to SEV1 and treat it as an outage.

---

## Version skew

**Alert:** `GoAgentsVersionSkew` — warning, `for: 10m`
**Expression:** `count by (job) (count by (job, version) (build_info{job="go-agents"})) > 1`

### What it means

More than one `build_info` **version** is serving. During a rollout this is
normal and expected: `maxSurge: 1` with `maxUnavailable: 0` means a new pod runs
alongside the old ones until it is ready. The `for: 10m` is five times the 120s
`progressDeadlineSeconds`, so past that it is stuck, not in progress.

`build_info` is always 1, so its value carries no signal; the signal is the
cardinality of its `version` label. That matters for a reason worth knowing:
with `maxUnavailable: 0` a bad image never becomes *unhealthy*, it simply never
becomes Ready. Error rate, latency and readiness all stay clean while the old
ReplicaSet keeps serving. **Two versions being up is the only evidence a rollout
is stuck.**

One blind spot: the rule counts `version`, which comes from the `VERSION` build
arg, and two images stamped with the *same* `VERSION` string are one row to it
however different their binaries. Both documented paths set `VERSION` to the
short commit SHA — `task kind:deploy`, and step 2 of
[deploy.md](deploy.md#2-build-the-image) — so it varies per commit. It does
**not** vary per build: on a dirty tree the task appends a timestamp to the
image *tag* but stamps `VERSION` as `<short-sha>-dirty`, so two dirty builds off
one commit are distinct images the skew rule cannot separate. The `commit` label
does not help either — it carries the short SHA without the suffix — so on a
dirty tree the per-ReplicaSet image listing in
[rollback.md](rollback.md#why-undo-actually-works-here) is the only place the
difference shows.

### Check first

```promql
count by (version, commit, env) (build_info{job="go-agents"})
```

Expect one row after a rollout completes; the row count tells you which versions
and the sample values how many replicas are on each.

```bash
kubectl --context kind-go-agents -n go-agents rollout status deployment/go-agents --timeout=10s
kubectl --context kind-go-agents -n go-agents get deployment go-agents \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl --context kind-go-agents -n go-agents get rs -l app.kubernetes.io/name=go-agents
```

`Progressing=False` with `ProgressDeadlineExceeded` confirms a stalled rollout.
Two ReplicaSets with non-zero replicas shows the split.

### Likely causes

- **The new pod cannot become ready**, so `maxUnavailable: 0` correctly refuses
  to remove the old one. The strategy working, not failing. Diagnose the new pod
  with [health-check-failure.md](health-check-failure.md).
- **`ErrImageNeverPull` on one node** — `imagePullPolicy: Never`, and
  `kind load` ran before that node existed, so only pods scheduled there fail
  and the skew is partial.
- **Someone ran `kubectl set image` or `scale` by hand** and the change is
  half-applied.

### Action

SEV3 while old pods are still serving — which, under `maxUnavailable: 0`, they
are. Fix the new pod and let the rollout finish, or clear the skew by abandoning
the new revision via [rollback.md](rollback.md).

`undo` does move the binary here: every build is tagged from its commit, so each
revision names a distinct image still on the node and restoring the previous
template restores the previous binary. The failure mode where `undo` restores an
identical template and changes nothing needs a *mutable* tag, which only happens
if someone deployed the `go-agents:dev` placeholder by hand — the committed
manifest carries it, and both documented deploy paths substitute it before
applying. [rollback.md](rollback.md#why-undo-actually-works-here) has the
per-ReplicaSet image check that tells the two apart in ten seconds.

A skew that includes a version nobody recognises means something deployed
outside the documented path. Find out what before you clear it.
