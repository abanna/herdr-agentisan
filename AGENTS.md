# AGENTS.md

Go service and CLI pair, structured so autonomous coding agents can change it
safely. Every rule below is enforced by a gate — if you break one, CI blocks
the merge.

## Bootstrap

```bash
go mod download && task build
uvx pre-commit install   # once per clone; wires the git pre-commit hook
```

`task check` additionally needs a running **Docker daemon**: the alert-rule
gate runs promtool from a pinned `prom/prometheus` image, because promtool has
no usable Go entrypoint (its module is the whole of Prometheus). Everything
else needs only Go and task. The gate refuses to run without docker rather than
skipping — a check that reports success on a machine that cannot run it is
worse than no check.

That is the whole bootstrap. Tool dependencies (golangci-lint, gosec,
govulncheck, modernize) are pinned in `go.mod` under the `tool` directive and
resolve through `go tool` — there is nothing to install globally and no
version to keep in sync by hand. `task tools` lists what is pinned.

Requires Go 1.26.6+ (pinned by the `toolchain` directive in `go.mod`; earlier
1.26 patches carry three reachable stdlib advisories that `govulncheck` fails
on) and [Task](https://taskfile.dev). `task --list` shows
everything.

## Commands

| Task | Command |
|------|---------|
| Lint | `go tool golangci-lint run ./...` |
| Format check | `go tool golangci-lint fmt --diff ./...` |
| Vet | `go vet ./...` |
| Run tests with a coverage profile | `go test -race -timeout 5m -coverpkg=./internal/... -coverprofile=coverage.out -covermode=atomic ./...` |
| Enforce the coverage floor | `go run ./cmd/devctl coverage --profile coverage.out --min 75` |
| Run the integration suite | `go test -tags=integration -race -timeout 5m ./...` |
| Static security scan | `go tool gosec -quiet ./...` |
| Secret scan (git history) | `go run github.com/zricethezav/gitleaks/v8@v8.30.1 git . --config .gitleaks.toml --no-banner --redact` |
| Dependency vulnerability check | `go tool govulncheck ./...` |
| Report modernisable constructs | `go tool modernize ./...` |
| Regenerate the API spec | `go run ./cmd/devctl openapi --write` |
| Check the spec is current | `git diff --exit-code docs/openapi.json` |
| Check docs match CI | `go run ./cmd/devctl docs-parity` |
| Check for oversized tracked files | `go run ./cmd/devctl large-files` |
| Check the deploy manifests match the code | `go run ./cmd/devctl manifests` |
| Lint the Prometheus alert rules | `docker run --rm -v "$PWD/infrastructure/local/observability/alerts:/rules" -w /rules --entrypoint promtool prom/prometheus:v3.6.0 check rules rules.yaml` |
| Unit-test the Prometheus alert rules | `docker run --rm -v "$PWD/infrastructure/local/observability/alerts:/rules" -w /rules --entrypoint promtool prom/prometheus:v3.6.0 test rules rules_test.yaml` |
| Run all pre-commit hooks | `uvx pre-commit run --all-files` |
| Tidy dependencies | `go mod tidy` |
| Check go.mod is tidy | `git diff --exit-code go.mod go.sum` |
| Run the API | `task run:server` |
| Run the CLI | `task run:cli` |
| Install the binaries | `task install` |
| Show pinned tool versions | `task tools` |
| Rebuild the tdaddy graph | `task tdaddy:index` |
| Which tests your change affects | `task tdaddy:impact` |

Every gate also has a task in `Taskfile.yml` running the **identical**
command. `task check` runs most of them as `deps`/`cmds`; `tidy` and
`modernize` are gates CI runs as their own jobs but are not part of `check`
(running `go mod tidy` or reporting modernizable constructs on every `task
check` would be surprising side effects for a read-mostly gate run). The
tasks are shortcuts, not a second source of truth — this table stays
byte-identical to `.github/workflows/ci.yml`, and the tasks invoke those same
strings. Change a gate and you change all three together.

`go run ./cmd/devctl docs-parity` enforces exactly that, in every direction: a
gate CI runs must appear here *and* in a task, and a command documented here
must be one CI runs unless it is declared local-only in
`internal/devcli/parity.go`. A command table that drifts from CI teaches the
next agent to "fix" a gate that was never broken.

The last rows are local-only: CI never starts a server, runs the
interactive CLI, or installs into a developer's `GOBIN` — and the tdaddy rows
need a binary CI cannot install (see below).

Two maps in `internal/devcli/parity.go` are escape hatches from this check:
`localOnly` (documented commands CI need not run) and `nonGateTasks` (tasks not
compared against CI at all). **Adding an entry to either weakens the gate.**
Every entry carries a stated reason; if a parity failure tempts you to add a
name, the fix is almost always to make CI and the docs agree instead.

## The local cluster

`infrastructure/local/` is a working environment, not example YAML. The same manifests
CI validates are the ones that run.

```bash
task kind:up        # create the cluster, deploy the stack and the app
task kind:deploy    # rebuild, reload the image, roll it out
task kind:smoke     # prove it works: scraped series, a real trace, a 401
task kind:down      # delete the cluster
```

- `infrastructure/local/kind/` — the cluster: one control plane, **two** workers. A one-node
  cluster would let the PodDisruptionBudget, the topology spread and
  `maxUnavailable: 0` all pass while meaning nothing.
- `infrastructure/local/k8s/` — the service. Checked by `devctl manifests` against the code.
- `infrastructure/local/observability/` — otel-collector, Prometheus, Grafana,
  Tempo (traces), Loki (logs) and Pyroscope (continuous profiles, scraped off
  the admin port by an Alloy DaemonSet — no application change), plus
  the alert rules in `infrastructure/local/observability/alerts/`.
- `docs/runbooks/` — what to do when one of those alerts fires.

Once it is up, `k8s:*` is how you reach it:

```bash
task k8s:status      # deployments, services and pods in both namespaces
task k8s:resources   # every object this repo owns — `get all` omits most of it
task k8s:ui          # port-forward every UI at once and print the URLs
task k8s:ui -- grafana tempo   # ...or just some
task k8s:logs        # follow both replicas
task k8s:events      # recent events, warnings last
task k8s:alerts      # every alert rule and its live state
task k8s:shell       # a shell in an app pod (the Debian runtime has one)
task k8s:profile -- cpu   # pull a pprof profile off the admin port
```

`k8s:profile` and the admin port are the same story: the Service deliberately
does not route 9090, so the task forwards the POD. That is also how Prometheus
scrapes it and how Alloy feeds Pyroscope.

**Every `kind` and `kubectl` invocation names the cluster and context
explicitly** (`--name go-agents`, `--context kind-go-agents`), and no task
here ever leaves your shell's current context pointed at this cluster. A
developer's current context is routinely some other cluster; a task that
relies on it is a task that eventually rolls a deployment somewhere it should
not. The one `kubectl config use-context` in this repository, in `kind:up`,
exists only to restore what `kind create cluster` overwrites — not to select a
context for anything else to rely on.

These tasks are exempt from docs parity via `nonGateTasks` in
`internal/devcli/parity.go` — CI has no cluster and must not acquire one to
satisfy a documentation check. The gates that DO run over this material are
`devctl manifests` and the promtool rule tests, both static and both compared
normally.

## Layout

- `cmd/go-agents/` — product CLI entrypoint. A thin shim; no logic.
- `cmd/devctl/` — development operations CLI entrypoint. A thin shim.
- `cmd/server/` — REST API entrypoint. Owns process lifecycle and graceful shutdown.
- `internal/notes/` — **the domain.** Validation and storage behind `Store`.
  The CLI and the API both drive this, so they cannot disagree.
- `internal/api/` — gin router, handlers, and the OpenAPI operations table.
  `api.go` binds routes; `spec.go` documents them; `SpecDrift()` proves the two agree.
- `internal/cli/` — the `go-agents` cobra tree. Parses, calls the domain, renders.
- `internal/devcli/` — the `devctl` cobra tree: the coverage floor and the
  docs-parity check, neither of which the Go toolchain provides.
- `internal/config/` — environment-backed config. No secret has a default.
  Also carries build provenance (`Version`, `Commit`, injected at link time):
  it is configuration resolved from the environment the binary was BUILT in,
  and keeping it in a package of its own made every caller import two.
- `internal/logging/` — zerolog construction and context plumbing.
- `internal/telemetry/` — OpenTelemetry tracing and Prometheus RED metrics.
  `/metrics` and `/debug/pprof` live on a **separate admin port** so a Service
  can decline to route them; `config.Validate` refuses to share one listener.
- `infrastructure/local/` — the local environment: kind cluster, Kubernetes
  manifests and the observability stack. `local/` because it is one environment
  among the several a real deployment would grow.
- `.devcontainer/` — the reproducible dev environment: Go 1.26.6 plus the
  four tools `go tool` does not resolve (Task, uv, kubectl, kind).
- `scripts/kind-smoke.sh` — proves the deployed stack works, not that it applied.
- `docs/openapi.json` — generated, committed, and drift-checked in CI.
- `docs/api.md` — prose companion to the spec. `docs/architecture.md` — why the
  pieces are shaped this way, and the deliberate limits.
- `docs/diagrams.md` — five diagrams generated from the actual code and
  manifests: system architecture, the observability signal pipeline, the
  deployment rollout lifecycle, the create-note request sequence, and the
  `task check` gate pipeline.

Tests live beside the code they cover, in `package <name>_test`, so they
exercise the exported surface an agent would actually call.

Integration tests are gated behind `//go:build integration` and run separately
(`task test:integration`). They start a real `http.Server` on an ephemeral
port and use the real `FileStore`, so they catch what in-process `httptest`
calls cannot: process wiring, the `Location` header a client follows,
persistence across requests, and graceful shutdown. Keep them out of the
default suite — they bind sockets and touch disk.

## Test impact (tdaddy)

`tdaddy` maps a change to the tests that actually exercise it, across package
boundaries a hand-picked `go test ./thatpackage/` would miss.

```bash
task tdaddy:index     # rebuild the graph (purges first, deliberately)
task tdaddy:impact    # what your working-tree changes affect
```

**Run it on demand, not on every edit — and do not add a PostToolUse hook.**
Measured in this repo: one impact query takes ~20s, while the entire test
suite (144 tests) runs in ~1.3s (~2.5s with `-race`). Asking which tests to
run costs about 15x more than running all of them, so an edit-loop hook is
pure tax here. The hooks tdaddy can install (`tdaddy hook claude|codex
install`) were tried and deliberately removed.

Selectivity is weak on the files you edit most: `notes.go` flags 43 of 144
tests (~30%) and `api.go` flags 67 (~47%) — neither narrows much. Only leaf
packages narrow usefully (`coverage.go` -> 27 of 144, ~19%). These counts will
keep moving as the suite grows; re-measure with `tdaddy impact --files <path>
--max-tests 0` rather than trusting the numbers here indefinitely.

tdaddy earns its keep on repositories whose suites take minutes; its cost is
roughly constant (BadgerDB open dominates), so the ratio is what decides. Use
it here for the occasional "what does this actually touch?" question --
especially the cross-package answer -- and run `task check` for everything else.

**It is advisory, never the gate.** `task check` is the gate.

Three traps, all encoded as comments in `Taskfile.yml`:

1. **Purge before reindexing.** An incremental index on an existing store
   roughly doubles it, and a bloated store has been observed to *shift* risk
   scores — it answers wrong, not slowly. `tdaddy:index` passes `--force`.
2. **Never pipe `tdaddy impact` through `tail`.** The staleness warning is in
   the header and the risk score is in the footer; `| tail -N` drops the one
   line that says whether the number can be trusted.
3. **`--max-tests` defaults to 50 and truncates** with only a trailing note.
   `tdaddy:impact` passes `--max-tests 0`.

`.tdad/` is gitignored (~100MB BadgerDB). If tdaddy hangs on exit it can orphan
`.tdad/data/LOCK`; check `pgrep tdaddy` and remove the lock before retrying.

## Rules

1. **Never commit a secret.** Config comes from the environment.
   `GO_AGENTS_API_TOKEN` has no default and `config.Load()` returns an error
   outside development rather than falling back — keep it that way. `.env` is
   gitignored; `.env.example` holds no secret values — only the API token and
   the OTLP endpoint are left blank, everything else is a safe local default.
2. **New code needs tests.** The floor is 75% total statements over
   `./internal/...`, enforced by `devctl coverage`. The `cmd/` packages are
   outside the denominator on purpose: they are process wiring with no logic
   (rule 3 keeps them that way), and counting them would let real gaps in the
   domain hide behind untestable `main` bodies. Do not lower it, and do not write assertion-free tests to
   clear it — a test that cannot fail is worse than no test.
3. **Business logic goes in `internal/notes/`, never in a handler or a
   command.** A rule implemented in a gin handler is a rule the CLI does not
   have. Handlers and commands parse, delegate and render.
4. **Return wrapped errors; map them with `errors.Is`.** Domain code returns
   `ErrNotFound`/`ErrInvalid` wrapped with `%w`. Transport maps those sentinels
   onto status codes — never by matching on error strings. `wrapcheck` enforces
   the wrapping.
5. **Add a route and document it in the same change.** `api.operations` in
   `spec.go` must cover every route `Router()` binds; `SpecDrift()` fails the
   build otherwise. Then regenerate the spec — never hand-edit it.
6. **Never edit files on `main`.** Branch first.
7. **Keep changes scoped.** No drive-by refactors of files unrelated to your task.

## Do not touch

- `docs/openapi.json` — generated. Regenerate with
  `go run ./cmd/devctl openapi --write`; never hand-edit.
- `go.sum` — regenerate with `go mod tidy`, never hand-edit.
- `.github/workflows/` — CI definitions. Changing a gate is a human decision.
- `.golangci.yml` — the enabled linter set is deliberate. Silence a specific
  finding with a `//nolint:<linter>` comment *and a reason*, never by removing
  a linter.
- `.claude/` and `.mcp.json` — the agentisan bundle, installed out-of-band and
  gitignored. Never `git add .` here.
