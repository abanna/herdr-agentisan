# Architecture

## The shape

Three binaries, one domain:

```
cmd/go-agents ─┐
cmd/server    ─┼─→ internal/notes (domain: validation + Store interface)
cmd/devctl    ─┘        ├─ MemStore   (in-process, used by the server and tests)
                        └─ FileStore  (JSON on disk, used by the CLI)
```

`cmd/*` packages are thin shims — signal handling, fang styling, process
lifecycle. No logic. That is what keeps them outside the coverage denominator
honest: there is nothing in them worth testing that running the binary does
not already prove.

The load-bearing decision is that **the CLI and the API share `internal/notes`
rather than the API being a second implementation**. A validation rule added to
`Draft.Validate` applies to both surfaces at once. `notes.Store` is the seam:
swapping the in-memory or file store for Postgres means implementing four
methods, not editing handlers or commands.

## Why two CLIs

`go-agents` is the product. `devctl` is the repository's own tooling, and it
exists because two of this repo's gates have no Go builtin:

- **Coverage floor.** `go test` has no `--cov-fail-under`. `devctl coverage`
  parses `go tool cover -func` and fails below the threshold.
- **Docs parity.** `devctl docs-parity` proves AGENTS.md, `Taskfile.yml` and
  `ci.yml` describe the same gates.

Keeping these in Go rather than a shell or Python script means the repository
needs no second language to run its own checks, and the checks are themselves
tested.

## The three drift guards

An agent's most expensive failure mode here is not writing a bug — it is
trusting a document that has quietly stopped being true. Three guards exist
specifically to make that impossible:

| Guard | Prevents |
|-------|----------|
| `devctl docs-parity` | A documented command that CI no longer runs. An agent runs it, sees it pass, and concludes the gate passes. |
| `api.SpecDrift()` | A route bound but undocumented. The generated spec would be incomplete *and still pass* the committed-file diff. |
| `git diff --exit-code docs/openapi.json` | A documented route whose spec was never regenerated. |

Each was verified by deliberately breaking it and watching it fail before being
trusted — a check that has never been seen failing is not evidence of anything.

## Configuration

`internal/config` resolves flags > environment > `.env` > defaults, and
validates. `GO_AGENTS_API_TOKEN` has no default: outside development,
`config.Load()` returns `ErrMissingSecret` rather than starting an
unauthenticated service. Failing at startup is the design, not a rough edge.

## Request path

```
signal ctx → http.Server (timeouts, ReadHeaderTimeout)
           → gin.Recovery
           → observers      (otelgin span, then RED metrics — injected, see below)
           → requestLogger  (zerolog into the request context, + trace_id/span_id)
           → requireToken   (constant-time compare, mutating routes only)
           → handler        (parse → domain → render)
           → Server.fail    (errors.Is → status; internal errors are logged, never echoed)
```

Reads are open; writes require a bearer token. Handlers never inspect error
strings — they match the domain's sentinel errors, so a reworded message
cannot silently change a status code.

## Two listeners

`cmd/server` binds two ports, and which routes live on which is a security
boundary rather than a layout preference:

| Port | Serves | Published |
|------|--------|-----------|
| `GO_AGENTS_HTTP_ADDR` (`:8080`) | the API, `/healthz`, `/readyz`, `/version` | yes |
| `GO_AGENTS_ADMIN_ADDR` (`:9090`) | `/metrics`, `/debug/pprof/*` | no — scraped in-cluster |

`/debug/pprof` on a public listener hands an attacker heap dumps and goroutine
stacks. Keeping it on a second port means the Service can decline to route it,
and `config.Validate` returns an error if the two addresses are equal so the
protection cannot be lost to a stray environment variable.

`internal/api` does not import `internal/telemetry`. Middleware arrives through
`api.WithObservers`, so the router stays independent of what observes it and a
test can build one without an exporter. The order is load-bearing: the tracing
middleware must run *before* `requestLogger` for the log line to carry the
`trace_id` that ties it to the span.

## The local cluster

`infrastructure/local/` is a working environment, not example YAML. `task kind:up` builds it and
`task kind:smoke` proves it, by reading values out of the running components rather
than by checking that `kubectl apply` exited 0. [docs/diagrams.md](diagrams.md)
has a rendered version of the ASCII diagram below, plus four more covering the
rest of the system.

```
                  ┌───────────────── namespace: go-agents ──────────────┐
                  │  Deployment go-agents  (2 replicas, 2 nodes)        │
  Service :80 ───►│    :8080 http    /healthz /readyz /version /v1/...  │
  (http only)     │    :9090 admin   /metrics /debug/pprof   NOT routed │
                  └──┬──────────┬──────────┬─────────────────┬──────────┘
        metrics      │  profiles │     logs │          traces│ OTLP/HTTP
     (scrape pod IP) │ (scrape   │ (K8s API)│                │
                     │  pod IP)  │          │                │
                  ┌──▼──────────▼──────────▼────────────────▼──────────┐
                  │ namespace: observability                           │
                  │                                                    │
                  │  Prometheus     Alloy (DaemonSet)   otel-collector  │
                  │      │            │        │              │        │
                  │      │            ▼        ▼              ▼        │
                  │      │          Loki   Pyroscope        Tempo      │
                  │      │            │        │              │        │
                  │      └────────────┴────────┴──────────────┘        │
                  │                       │                            │
                  │                    Grafana                         │
                  │        (4 datasources, cross-linked)               │
                  └────────────────────────────────────────────────────┘
```

Four signals, one place to look. The links between them are the point:

| From | To | How |
|------|----|-----|
| a log line | its trace | Loki derived field regexes `trace_id` out of the raw line |
| a span | its logs | Tempo `tracesToLogsV2`, matched on namespace + app + trace id |
| a span | its RED metrics | Tempo `tracesToMetrics` against `http_request_duration_seconds` |

**Pyroscope needed no application change.** Alloy scrapes the `/debug/pprof`
endpoints that were already on the admin port, which is the second thing that
port has now paid for. Nothing in `internal/` knows Pyroscope exists.

**`trace_id` is in the log LINE, never a Loki label.** A per-request label value
multiplies streams rather than adding entries: it would mint one index entry per
request, an index larger than the data, and queries that time out. Every indexed
label is pod-bounded. `task kind:smoke` asserts that `trace_id` is *absent* from
`/loki/api/v1/labels`, so the mistake cannot be made quietly.

Three things about that picture are load-bearing rather than incidental:

- **The admin port has no Service.** Prometheus reaches `:9090` by scraping the pod
  IP, which needs no Service; nothing else in the cluster can route to it.
  `devctl manifests` fails the build if a Service ever grows that port.
- **`maxUnavailable: 0` plus a hard topology spread.** A new pod must pass readiness
  before an old one goes, and the two replicas must sit on different nodes —
  `whenUnsatisfiable: DoNotSchedule`, because `ScheduleAnyway` is only a scheduler
  score and was observed putting both replicas on one node. That is what makes the
  PodDisruptionBudget mean something, and what makes `rollout undo` a recovery step
  rather than a hope.
- **Every build gets its own image tag.** Under one mutable tag with
  `imagePullPolicy: Never`, every revision names the same string and `rollout undo`
  restores a pod template identical to the running one — reporting success and
  changing nothing.

The cluster is three nodes for the same reason: on one node the disruption budget,
the spread and the rolling update would all pass while proving nothing.

## Deliberate limits

- **`MemStore` and `FileStore` are not production storage.** `FileStore`
  rewrites the whole file under a mutex; correct for one developer on one
  machine, and honest about its ceiling. Concurrent writers across processes
  would need a lock file or a database.
- **Tracing exports only when told to.** `GO_AGENTS_OTLP_ENDPOINT` is empty by
  default, so `task run:server` on a laptop records no spans and needs no
  collector. W3C propagation is installed either way: a service that cannot
  export its own spans should still pass a trace context to the next hop.
- **The OpenAPI document is hand-modelled** in `spec.go` rather than derived
  from struct tags. `SpecDrift()` is what keeps that honest.
- **`MemStore` means the two replicas do not share state.** A note created on one
  is invisible to the other, and a restart loses both. That is a real operational
  caveat the runbooks call out, not a detail: it is the reason a read-after-write
  through the Service can 404. Fixing it is implementing `notes.Store` against a
  database, which is the seam the whole design exists to keep cheap.
- **No Alertmanager.** The rules are evaluated and their state is real —
  `GoAgentsAbsent` was driven pending-to-firing against the live cluster by
  breaking the scrape target — but nothing routes an alert anywhere yet, so they
  surface only in the Prometheus UI.
- **No circuit breakers, and that is not an omission.** This service makes zero
  outbound calls. Adding a dependency in order to justify a breaker would be
  worse than not having one.
