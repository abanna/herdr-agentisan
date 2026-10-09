# Deploy

**Use this** to get a new build of `go-agents` running in the local kind
cluster. **It assumes** the kind cluster `go-agents` already exists, you are
at the repository root on the commit you intend to ship, and `task check`
passes on it. Deploying a commit that has not passed the gates is how a broken
image reaches a cluster in the first place.

Deploying wipes every note in both replicas. See the MemStore caveat in
[README.md](README.md#the-memstore-caveat--read-this-before-you-diagnose-anything).

## The short version

```bash
task kind:deploy
```

That builds the image under an immutable, commit-derived tag, loads it onto
every node, applies the manifests and waits for the rollout. The rest of this
file is the same thing step by step, which is what you want when one of those
steps is the one going wrong.

## 1. Confirm the Secret exists

`GO_AGENTS_ENV` is `staging` in the manifest, and `config.Load()` refuses to
start without `GO_AGENTS_API_TOKEN`. A missing Secret is not a subtle failure:
the container exits 1 immediately and the pod enters `CrashLoopBackOff` with
`fatal: load config: required secret is not set: GO_AGENTS_API_TOKEN` in its
logs.

```bash
task kind:secret
```

That creates the namespace and, only if the Secret is absent, generates a token
from local randomness. It is create-if-absent deliberately — regenerating would
rotate the token and restart both pods — so against a healthy cluster it prints
`secret go-agents-api-token already exists` and changes nothing. Running it
*is* the confirmation.

Without Task, the same thing by hand. `/dev/urandom` rather than `openssl`,
which these runbooks do not list as a prerequisite and the task does not use:

```bash
kubectl --context kind-go-agents apply -f infrastructure/local/k8s/namespace.yaml
kubectl --context kind-go-agents -n go-agents get secret go-agents-api-token

# Only if that reported NotFound. Never a value written down anywhere in this repo.
kubectl --context kind-go-agents -n go-agents create secret generic go-agents-api-token \
  --from-literal=token="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
```

## 2. Build the image

Provenance is passed in as build args because the Docker build context has no
`.git`, so `debug.ReadBuildInfo` would stamp nothing and `/version` would report
an empty commit — which leaves the verification in step 6 with nothing to
compare.

Tag by commit, not `:dev`. That is what makes `rollout undo` a real rollback
rather than a restart — see [rollback.md](rollback.md#why-undo-actually-works-here).

**And add a suffix when the tree is dirty.** `git rev-parse --short HEAD` does
not move until you commit, so on a dirty tree every rebuild reuses the same tag.
The pod template is then byte-identical to the running one, `apply` reports
`configured`, no ReplicaSet is created, and the old binary keeps serving. That
is the exact trap this manual path exists to walk you through — and you are most
likely to be here while iterating on uncommitted code. `TAG` below mirrors what
`task kind:deploy` does.

```bash
SHA="$(git rev-parse --short HEAD)"
TAG="$SHA"
VERSION="$SHA"
if [ -n "$(git status --porcelain)" ]; then
  TAG="${SHA}-dirty-$(date -u +%H%M%S)"
  VERSION="${SHA}-dirty"
fi
docker build \
  --build-arg VERSION="$VERSION" \
  --build-arg COMMIT="$SHA" \
  -t "go-agents:$TAG" .
```

Expect a successful build ending in the Debian (`debian:trixie-slim`) runtime stage. Keep `$TAG`
for steps 3 and 4, and `$SHA` for step 6 — `/version` reports the COMMIT, which
is deliberately the same across dirty rebuilds, so it confirms which commit is
serving and cannot tell you which build is.

## 3. Load the image onto the cluster nodes

`imagePullPolicy: Never` means the kubelet will never contact a registry. The
image has to be *on the node*, and `kind load` is what puts it there.

```bash
kind load docker-image "go-agents:$TAG" --name go-agents
```

Expect one "Image: ... with ID ... not yet present on node ..., loading..." line
per node, or silence if a node already has that exact image ID.

If a worker node joined the cluster after a previous load, it does **not** have
the image, and pods scheduled there fail with `ErrImageNeverPull`. Re-running
this command fixes that; it loads to every current node.

## 4. Apply the manifests with that tag

The committed manifest carries the placeholder `go-agents:dev`. Render,
substitute, apply — in **one** step. The `grep` is not optional: a renamed image
in the manifest makes the `sed` a no-op, and applying an unguarded no-op deploys
the placeholder tag, which nothing else here would tell you until step 6:

```bash
PATCHED="$(kubectl kustomize infrastructure/local/k8s | sed "s|image: go-agents:dev$|image: go-agents:$TAG|")"

if printf '%s' "$PATCHED" | grep -q "image: go-agents:$TAG"; then
  printf '%s' "$PATCHED" | kubectl --context kind-go-agents apply -f -
else
  echo "substitution did not match: infrastructure/local/k8s no longer carries image: go-agents:dev" >&2
fi
```

(`kubectl kustomize` renders locally and contacts no cluster, so it takes no
`--context`. Only the `apply` does.)

Expect `namespace/go-agents configured`, `deployment.apps/go-agents
created` or `configured`, `service/…`, `poddisruptionbudget.policy/…`.

**Do not `apply -k` and then `set image`.** It looks equivalent and is not:
`apply` writes the placeholder tag back first, so each deploy produces *two*
Deployment revisions and `rollout undo` lands on the intermediate one carrying
`go-agents:dev`. That was measured here, not theorised.

The other trap this avoids: with a mutable tag, a rebuild leaves the pod
template byte-identical, Kubernetes sees no change, creates no ReplicaSet, and
the running pods keep serving the *old* binary while `apply` reports
`configured`. A distinct tag per build makes the template change, so the
rollout starts on its own and no `rollout restart` is needed.

## 5. Watch the rollout, and tell completed from stalled

```bash
kubectl --context kind-go-agents -n go-agents rollout status deployment/go-agents --timeout=150s
```

The timeout is 150s deliberately: `progressDeadlineSeconds` is 120, so the
Deployment gives up first and `rollout status` reports the *reason* rather than
a client-side timeout that tells you nothing.

- **Completed** — `deployment "go-agents" successfully rolled out`, exit 0.
- **Stalled** — `error: deployment "go-agents" exceeded its progress
  deadline`, exit 1. The rollout stopped making progress for 120s.
- **Still going** — the command is still printing `Waiting for deployment
  "go-agents" rollout to finish: N of 2 updated replicas are available…`.

`maxUnavailable: 0` is what makes "stalled" survivable: a new pod must pass its
readiness probe before an old one is removed, so a broken image **stalls the
rollout instead of taking the service down**. If the rollout stalled, the old
pods are still serving traffic. You have time. Do not panic-rollback before
reading the reason.

If it stalled, get the reason from the Deployment conditions and the new pods:

```bash
kubectl --context kind-go-agents -n go-agents get deployment go-agents \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'

kubectl --context kind-go-agents -n go-agents get pods -l app.kubernetes.io/name=go-agents
kubectl --context kind-go-agents -n go-agents describe pod -l app.kubernetes.io/name=go-agents | tail -40
```

| What you see | What it means | Where to go |
|--------------|---------------|-------------|
| `Progressing=False ProgressDeadlineExceeded` | new pods never became ready in 120s | pod state below |
| `ErrImageNeverPull` | the image is not on that node | back to step 3 |
| `CrashLoopBackOff`, exit 1 at startup | config rejected, usually the missing Secret | step 1, then `kubectl logs` |
| `Running` but `0/1 READY` | readiness never passed | [health-check-failure.md](health-check-failure.md) |
| `OOMKilled` in `lastState` | exceeded the 128Mi memory limit | [on-call-triage.md](on-call-triage.md#saturation) |

To abandon a stalled rollout, go to [rollback.md](rollback.md). Because the old
pods are still serving, this is a decision, not an emergency.

## 6. Verify the new build is actually serving

Two checks, and you need both. `/version` proves a pod is running the new
binary; `build_info` proves *every* pod is.

**Per pod — `/version` reports the new commit.** Note that
`kubectl port-forward svc/…` pins to a *single* backing pod and does not
load-balance, so this answers for one replica only:

```bash
kubectl --context kind-go-agents -n go-agents port-forward svc/go-agents 8080:80
# in another shell:
curl -s localhost:8080/version
```

Expect `{"version":"…","commit":"<the short SHA from step 2>"}`. A different
commit means that pod is old, and the rollout has not finished — re-run step 5.

**This check cannot tell you the build is new, only the commit.** On a dirty
tree `/version` reports the same commit for every rebuild, so it reads exactly
the same whether the new bits landed or `apply` was a no-op. The tag is what
distinguishes them:

```bash
kubectl --context kind-go-agents -n go-agents get deployment go-agents \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

That must equal the `go-agents:$TAG` you built in step 2. If it is
`go-agents:dev`, step 4's guard did not fire and the placeholder was applied
— which step 4 refuses to do, so you are looking at an older deploy that
predates the guard.

**Fleet-wide — `build_info` collapses to one commit.** In Prometheus (see
[incident-response.md](incident-response.md#open-the-signals) for the
port-forward):

```promql
count by (version, commit, env) (build_info)
```

Expect exactly **one** row, with the new commit, and a value of 2 once both
replicas are up. Two rows means two commits are serving at once: normal for the
duration of a rollout, and a stalled rollout after that. This is the same
condition the version-skew alert watches — see
[on-call-triage.md](on-call-triage.md#version-skew).

On the dashboard, `build_info` is the deploy marker: overlay it on the latency
and error-rate panels so "when the graph moved" lines up with "what changed".

## 7. Smoke-test the API

Reads need no token, so verify with those first:

```bash
curl -s localhost:8080/healthz   # {"status":"ok"}
curl -s localhost:8080/readyz    # {"status":"ready"}
curl -s localhost:8080/v1/notes   # [] on a fresh pod — MemStore starts empty
```

If you need to verify a write, source the token from the cluster rather than
typing one. Never echo it, never paste it into a ticket:

```bash
TOKEN="$(kubectl --context kind-go-agents -n go-agents \
  get secret go-agents-api-token -o jsonpath='{.data.token}' | base64 -d)"

# -i, because the status line and the Location header are what you are checking
# and plain `curl -s` prints only the body.
curl -s -i -X POST localhost:8080/v1/notes \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"title":"deploy smoke","body":"ok"}'
```

Expect `HTTP/1.1 201 Created` and a `Location` header. Reading it back through
the Service may `404` — the port-forward pins to one pod, but a fresh `curl`
through a real Service endpoint can land on the other replica, which has never
seen that note. That is the MemStore caveat, not a failed deploy.

## Recording why a revision exists

Every build already gets its own tag, so `rollout history` shows distinct
images. What it does not show is *why*. `kubectl --record` was removed and
nothing replaced it, so set the annotation yourself when a deploy is one you
might have to explain later:

```bash
kubectl --context kind-go-agents -n go-agents annotate deployment/go-agents \
  "kubernetes.io/change-cause=deploy $SHA: <one line on what changed>" --overwrite
```

It shows up in the CHANGE-CAUSE column:

```bash
kubectl --context kind-go-agents -n go-agents rollout history deployment/go-agents
```

Six revisions later this is the difference between a rollback decision that
takes ten seconds and one that takes ten minutes of `git log`.
