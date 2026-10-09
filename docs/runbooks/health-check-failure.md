# Health check failure

**Use this** when a pod is restarting, never becomes `1/1 READY`, or has dropped
out of the Service endpoints. It is the `runbook_url` target of
`GoAgentsReadinessFailing`. **It assumes** the pod exists and is scheduled —
if it is `Pending` or `ErrImageNeverPull` (the only pull failure this cluster's
`imagePullPolicy: Never` can produce; there is no registry pull to back off
from, so `ImagePullBackOff` cannot happen here), the problem is scheduling or
the image, and [deploy.md](deploy.md#5-watch-the-rollout-and-tell-completed-from-stalled)
has that table.

## `/healthz` and `/readyz` are not the same question

| | `/healthz` | `/readyz` |
|---|---|---|
| Handler does | returns `{"status":"ok"}` unconditionally | calls `store.List(ctx)`, then returns `{"status":"ready"}` |
| Answers the question | "is this process alive?" | "should traffic be sent here?" |
| Used by | `startupProbe`, `livenessProbe` | `readinessProbe` |
| Failure consequence | the pod is **restarted** | the pod is **removed from Endpoints** |

Collapsing them is the usual mistake, and the reason they are separate here is
the consequence column: a liveness probe that touches the store restarts the pod
whenever a dependency is slow, turning a degradation into an outage.

### What a failure of each actually means *here*

**`/healthz` failing** cannot mean "a dependency is unhappy" — the handler does
not consult anything. It has exactly three causes: the process is not listening
yet (still starting), the process is wedged so the HTTP server cannot answer
within the 2s probe timeout, or the process is gone (crashed, OOMKilled,
exiting).

**`/readyz` failing is the surprising one.** The store is `notes.MemStore`, and
`MemStore.List` returns an error only when the *request context is already
cancelled*. Nothing else in it can fail: it takes a read lock, copies a map and
sorts it. So a `503 {"status":"unavailable"}` from `/readyz` is very nearly
unreachable while the process is healthy — **there is no store dependency here
that can go down.** In practice a readiness probe failure means the kubelet got
no answer at all, for the same three reasons `/healthz` fails.

The one case that genuinely separates them: `/readyz` does strictly more work
than `/healthz`, so a process under enough pressure to exceed the 2s probe
timeout on `List` but not on `health` will fail readiness while liveness still
passes. That is the pod telling you it is saturated, not broken —
[on-call-triage.md](on-call-triage.md#saturation).

Do not go looking for a failed backing store. There isn't one. When this service
is unready, it is because the *process* is not answering.

This has a consequence for the alert that sent you here.
`GoAgentsReadinessFailing` keys on the ratio of 503s among `/readyz`
responses, so it can only fire in the narrow case above — a process slow enough
to blow the 2s probe timeout while still being scraped. The common unready
cases, where the pod produces no `/readyz` samples at all, are **silent to that
alert** and surface as `GoAgentsAbsent` instead. Trust `kubectl get pods`
over the alert set:
[on-call-triage.md](on-call-triage.md#readiness-failing) has the full reasoning.

## The probe configuration, and what it buys you in time

From `infrastructure/local/k8s/deployment.yaml`. The arithmetic is what tells you whether a
pod is "still starting" or "actually stuck", so do it rather than eyeballing:

| Probe | Path | Period | Timeout | Failures | Budget before action |
|-------|------|--------|---------|----------|----------------------|
| `startupProbe` | `/healthz` | 2s | (default 1s) | 15 | **30s** to first success, then it stops |
| `livenessProbe` | `/healthz` | 10s | 2s | 3 | **~30s** of no answer before restart |
| `readinessProbe` | `/readyz` | 5s | 2s | 2 | **~10s** of no answer before removal from Endpoints |

Three things follow:

- **Liveness and readiness do not run until the startup probe succeeds.** A pod
  in its first 30 seconds cannot be restarted by liveness, no matter how
  unhealthy it looks. If a pod is 20 seconds old and not ready, wait.
- **A pod fails out of the Service three times faster than it gets restarted**
  (10s vs 30s). That ordering is deliberate: stop sending traffic first, restart
  only if the process is genuinely wedged.
- **`terminationGracePeriodSeconds: 30` against a 10s
  `GO_AGENTS_SHUTDOWN_TIMEOUT`** leaves 20s of headroom for the endpoints
  controller to stop routing to a terminating pod. A pod in `Terminating` that
  is briefly unready is shutdown working, not a failure.

## Diagnosis order

**1. Find out which probe is failing, and whether the container is even up.**

```bash
kubectl --context kind-go-agents -n go-agents get pods -l app.kubernetes.io/name=go-agents -o wide
kubectl --context kind-go-agents -n go-agents describe pod <name> | tail -30
```

The Events tail names the probe verbatim: `Startup probe failed:`,
`Liveness probe failed:`, `Readiness probe failed:`, each with the HTTP status
or the connection error. That one line decides everything below.

```bash
kubectl --context kind-go-agents -n go-agents get pod <name> \
  -o jsonpath='{.status.containerStatuses[0].restartCount}{"  "}{.status.containerStatuses[0].lastState}{"\n"}'
```

| What you see | What it means | Next |
|---|---|---|
| `restartCount: 0`, pod < 30s old, not ready | still inside the startup budget | wait 30s and re-check |
| `lastState` has `reason: OOMKilled` (exit 137) | exceeded the 128Mi memory limit | [saturation](on-call-triage.md#saturation), and take a heap profile |
| `lastState` exit code 1, restarts climbing fast | the process refuses to start — config | step 2 |
| `Liveness probe failed: … connection refused` | the process is not listening | step 2 |
| `Liveness probe failed: … Client.Timeout` | the process is wedged, not dead | step 4 |
| `Readiness probe failed:` alone, liveness fine | slow `/readyz` under pressure | [saturation](on-call-triage.md#saturation) |
| Pod is `Terminating` | shutdown draining, up to 30s | not a failure |

**2. Read what the process said.** A container that exits at startup logs the
reason and then there is nothing to probe.

```bash
kubectl --context kind-go-agents -n go-agents logs <name> --tail=50
kubectl --context kind-go-agents -n go-agents logs <name> --previous --tail=50
```

`--previous` is the important one for a `CrashLoopBackOff`: the current
container may not have started yet, and the message you need is from the one
that died.

Expect a healthy start to log `server listening` with `addr`, `admin_addr`,
`env`, `version`, `commit` and `auth_enabled: true`. Instead you may see:

- `fatal: load config: required secret is not set: GO_AGENTS_API_TOKEN` — the
  `go-agents-api-token` Secret is missing or has no `token` key.
  `GO_AGENTS_ENV` is `staging`, so the service refuses to start
  unauthenticated. Fix per [deploy.md](deploy.md#1-confirm-the-secret-exists),
  then `rollout restart`. **Not** a rollback:
  [rollback.md](rollback.md#when-rollback-is-not-the-right-move).
- `fatal: load config: admin_addr … must differ from http_addr` — someone set
  the two listen addresses to the same value. The config refuses rather than
  publishing `/debug/pprof` on the user-facing port.
- `fatal: listen api on :8080: … address already in use` — two processes in one
  pod, essentially only possible from a hand-edited manifest.
- **Nothing at all** — the container never got far enough to build a logger.
  Check the image is what you think it is:

  ```bash
  kubectl --context kind-go-agents -n go-agents get pod <name> \
    -o jsonpath='{.spec.containers[0].image}{"\n"}'
  ```

**3. Ask the endpoints yourself.** Bypass the kubelet and probe the pod
directly, so you can tell "the probe is misconfigured" from "the process is not
answering". Use the pod, not the Service — you want *this* pod's answer.

```bash
kubectl --context kind-go-agents -n go-agents port-forward pod/<name> 8080:8080
# in another shell:
curl -s -i localhost:8080/healthz
curl -s -i localhost:8080/readyz
```

- Both `200` — the process is fine and the probe's view differs from yours.
  Suspect a probe pointed at the wrong port (`port: http` must resolve to
  containerPort 8080), or the pod being CPU-starved only under real traffic.
- `/healthz` `200`, `/readyz` `503` — the request context was already cancelled.
  In practice this means the process is too slow to answer within the deadline;
  go to [saturation](on-call-triage.md#saturation).
- Connection refused, or the port-forward itself fails — the process is not
  listening. Back to step 2.
- Both hang — the process is wedged. Step 4.

**4. If the process is wedged, profile it before you restart it.** A restart
destroys the only evidence, and with `MemStore` it also destroys that replica's
data. The admin port is on 9090 and still answers even when the API listener is
saturated, because it is a separate `http.Server`:

```bash
kubectl --context kind-go-agents -n go-agents port-forward pod/<name> 9090:9090
curl -s 'http://localhost:9090/debug/pprof/goroutine?debug=1' | head -60
```

A goroutine count in the thousands, or many goroutines parked on the same lock,
is the answer. Full procedure in
[incident-response.md](incident-response.md#pulling-a-pprof-profile).

**5. Only then restart.**

```bash
kubectl --context kind-go-agents -n go-agents delete pod <name>
```

Deleting one pod is the smaller hammer: the ReplicaSet replaces it, the PDB
(`minAvailable: 1`) is not involved because this is not a voluntary eviction,
and the other replica keeps serving. `rollout restart` cycles *both* and wipes
both MemStores; save it for a change that has to reach every pod.

## If both pods are unready

The Service has no endpoints and every client gets a connection error. This is
the SEV1 case in [incident-response.md](incident-response.md#severity).

Both replicas failing identically almost always means something they *share*:
the Secret, the image, the manifest, or the nodes. It is very unlikely to be two
independent process failures.

```bash
kubectl --context kind-go-agents get nodes
kubectl --context kind-go-agents -n go-agents get events --sort-by=.lastTimestamp | tail -20
```

Work step 2 on either pod — whatever they share will be in both sets of logs —
and if a deploy immediately preceded it, [rollback.md](rollback.md).
