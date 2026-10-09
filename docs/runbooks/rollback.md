# Rollback

**Use this** when a deploy made things worse and you want the previous state
back. It is the `runbook_url` target of `GoAgentsVersionSkew`. **It assumes**
the Deployment `go-agents` exists in namespace `go-agents` and that the
problem started at a deploy — if you are not sure it did, start at
[incident-response.md](incident-response.md) instead.

## The command

```bash
kubectl --context kind-go-agents -n go-agents rollout undo deployment/go-agents
```

That is the whole rollback. It reverts the Deployment to the previous revision
and rolls the change out under the same `maxSurge: 1 / maxUnavailable: 0`
strategy as any other deploy. Watch it land:

```bash
kubectl --context kind-go-agents -n go-agents rollout status deployment/go-agents --timeout=150s
```

Everything below is about picking a different revision, verifying the result,
knowing when `undo` cannot help, and knowing when rolling back is the wrong
move.

## Why `undo` actually works here

`task kind:deploy` builds and loads every image under its **own** tag, derived
from the commit (`go-agents:<short-sha>`, plus a `-dirty-<time>` suffix when
the tree is not clean). Each Deployment revision therefore names a distinct
image that is still on the node, which is what `undo` needs: it restores the
previous template, that template names the previous image, and the previous
binary comes back.

**This is the whole reason for the per-build tag.** Under one mutable tag such
as `go-agents:dev`, plus `imagePullPolicy: Never`, every revision names the
same string and the kubelet uses whatever that tag currently points at *on the
node*. `undo` then restores a template identical to the running one, new pods
start, and they start on the **new** binary. The rollback reports success and
changes nothing. If you ever see revisions that all name the same image, you are
in that state:

```bash
kubectl --context kind-go-agents -n go-agents get replicaset \
  -l app.kubernetes.io/name=go-agents \
  -o 'custom-columns=RS:.metadata.name,IMAGE:.spec.template.spec.containers[0].image'
```

Distinct images per ReplicaSet means `undo` will do what you want. Identical
images mean it will not, and you should rebuild the previous commit instead:

```bash
git log --oneline -5                       # find the commit you were on
git checkout <previous-good-sha>
task kind:deploy                           # tags from THAT commit
```

If you take one thing from this file, take this: check what the current and
previous revisions actually name before trusting `undo` to change the binary.

```bash
# What is running now.
kubectl --context kind-go-agents -n go-agents get deployment go-agents \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# What the revision you would land on names. Take <N> from `rollout history`
# below; the previous revision is the second-highest number in that table.
kubectl --context kind-go-agents -n go-agents rollout history deployment/go-agents --revision=<N> \
  | grep -i 'image:'
```

Two different strings means `undo` will change the binary. The same string
twice means it will not.

## Rolling back to a specific revision

```bash
kubectl --context kind-go-agents -n go-agents rollout history deployment/go-agents
```

Expect a table of `REVISION` and `CHANGE-CAUSE`. `CHANGE-CAUSE` is `<none>`
unless someone set the `kubernetes.io/change-cause` annotation on that deploy —
[deploy.md](deploy.md#recording-why-a-revision-exists) explains how.
Inspect one before committing to it:

```bash
kubectl --context kind-go-agents -n go-agents rollout history deployment/go-agents --revision=3
```

Expect the full pod template for revision 3. Read the `Image:` and the
`Environment:` lines and satisfy yourself it is the state you want.

```bash
kubectl --context kind-go-agents -n go-agents rollout undo deployment/go-agents --to-revision=3
```

Revision history is bounded by `revisionHistoryLimit`, which is unset and so
defaults to 10. A revision older than that is gone from the cluster, and the
only way back to it is a rebuild from git.

## Verify the rollback landed

Three checks, in this order.

**1. The rollout completed.**

```bash
kubectl --context kind-go-agents -n go-agents rollout status deployment/go-agents --timeout=150s
```

Expect `successfully rolled out`. If it stalls with
`ProgressDeadlineExceeded`, the revision you rolled back *to* cannot become
ready either — go to [health-check-failure.md](health-check-failure.md) rather
than rolling back again.

**2. Every pod reports the expected commit.** In Prometheus:

```promql
count by (version, commit, env) (build_info)
```

Expect one row, showing the commit you rolled back to, with value 2. Two rows
means the rollback is still in progress or has stalled part-way.

**3. The symptom is gone.** Whatever sent you here — error rate, latency,
readiness — recheck the same panel or query. A rollback that completed cleanly
while the graph stayed bad means the deploy was not the cause, and you have
spent the change budget without buying anything. Go back to
[incident-response.md](incident-response.md).

## When rollback is NOT the right move

Rolling back is cheap here, but it is not free: it restarts every pod, which
wipes both MemStores, and it consumes the "we changed something" hypothesis. Do
not reach for it when:

- **The rollout stalled and old pods are still serving.** `maxUnavailable: 0`
  means a failed rollout never removed a healthy pod. There is no outage. Read
  the failure reason first — the fix is usually a missing image on a node or a
  missing Secret, and rolling back hides the evidence.
- **The problem is data, not code.** There is nothing to roll back to: storage
  is an in-process `MemStore`, notes never persisted, and a rollback deletes
  what is left. A `404` on a note that was just created is the two replicas not
  sharing state, not a regression. See
  [README.md](README.md#the-memstore-caveat--read-this-before-you-diagnose-anything).
- **The problem is the Secret or the environment.** The `go-agents-api-token`
  Secret is not part of the Deployment revision, so `undo` restores a template
  that references the same broken Secret. Fix the Secret and
  `rollout restart` instead.
- **The problem is below the Deployment.** A node `NotReady`, an image never
  loaded onto a node, the kind cluster itself unhealthy: the previous revision
  is equally affected. `kubectl --context kind-go-agents get nodes` before
  you decide.
- **The previous revision has the same bug.** If the regression was introduced
  three deploys ago and only became visible under today's traffic, `undo` moves
  you to an equally broken build and destroys the correlation you were about to
  find in `build_info`.
- **The observability stack is what is broken.** No rule watches the
  `observability` namespace, so Prometheus, Grafana, Tempo, Loki, Pyroscope, Alloy or the collector
  failing does not page — it shows up as panels going blank and queries
  returning nothing. Losing the ability to watch the application says nothing
  about the application. Do not roll it back because the graphs went empty.
