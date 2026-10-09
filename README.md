# go-agents

A Go service and CLI pair, structured so autonomous coding agents can change it
safely. Three binaries over one domain:

| Binary | Purpose |
|--------|---------|
| `go-agents` | Product CLI — create, list, read and delete notes |
| `server` | REST API over the same domain |
| `devctl` | Development operations: coverage floor, docs parity, spec generation |

The CLI and the API both drive `internal/notes`, so the two surfaces cannot
disagree about a rule.

## Quick start

```bash
go mod download && task build

task run:server                       # REST API on :8080
task run:cli -- notes add "first" -b "hello"
```

```bash
curl -s localhost:8080/healthz
curl -s -X POST localhost:8080/v1/notes \
  -H 'Content-Type: application/json' \
  -d '{"title":"from curl","body":"hello"}'
curl -s localhost:8080/v1/notes
```

Requires Go 1.26.6+ and [Task](https://taskfile.dev). Linters and scanners are
pinned in `go.mod` under the `tool` directive — nothing to install globally.

## Gates

`task check` runs most of what CI runs: lint, format, vet, gosec, gitleaks,
govulncheck, race-enabled unit and integration tests, the coverage floor, the
manifests and alert-rule checks, the OpenAPI drift check, the docs parity
check, and the pre-commit hooks. `go mod tidy` and `go tool modernize` are
separate CI jobs (`task tidy`, `task modernize`) not included in `check` — see
the command table in [AGENTS.md](AGENTS.md#commands) for the full, CI-verified
list.

Two of those have no Go builtin and are implemented in `internal/devcli`:

- **Coverage floor** — `go test` has no `--cov-fail-under`, so `devctl
  coverage` parses `go tool cover -func` and fails under 75%.
- **Docs parity** — `devctl docs-parity` proves the AGENTS.md command table,
  `Taskfile.yml` and `.github/workflows/ci.yml` describe the same gates, in
  every direction. A command table that drifts from CI teaches the next agent
  to "fix" a gate that was never broken.

A third guard, `api.SpecDrift()`, fails the build when a route is bound
without being documented — otherwise the generated spec would be incomplete
and still pass the committed-file diff.

## Diagrams

Five diagrams generated from the actual code and manifests — system
architecture, the observability signal pipeline, the deployment rollout
lifecycle, the create-note request sequence, and the `task check` gate
pipeline — in [docs/diagrams.md](docs/diagrams.md).

## Agent instructions

`AGENTS.md` is canonical. `CLAUDE.md` is a single `@AGENTS.md` import so
Claude Code picks it up without creating a second source of truth.

## Layout

See the Layout section of [AGENTS.md](AGENTS.md).

## Dev container

`.devcontainer/` builds the local workflow into one image: Go 1.26.6, Task, uv
(which is how `uvx pre-commit` runs), and kubectl/kind for the local cluster.
Everything `go tool` already resolves — golangci-lint, gosec, govulncheck,
modernize, gitleaks — is deliberately absent; go.mod is where those are pinned.

Each version is pinned in `.devcontainer/Dockerfile`, checked against the
publisher's sha256, and then run with `--version` in the same layer. A dead
pin, a renamed asset or a truncated download fails `docker build` rather than
surfacing an hour later as a confusing error.

`devcontainer.json` is strict, comment-free JSON on purpose. The devcontainer
spec permits JSONC, but the `check-json` hook runs over every `.json` file with
no exclusions, so a comment there fails the repo's own gate. The reasoning
lives in the Dockerfile comments and in this section instead.

The editor settings are part of that reasoning. `go.lintOnSave` is `"off"`
because golangci-lint is a `go tool` pinned in `go.mod`, not a binary on PATH
the Go extension can resolve; leaving it on produces a "tool not found" error
on every save and tempts the next reader to install a second, unpinned copy.
`task lint` owns linting, and the `lint` pre-commit hook runs the same target.
`gopls.formatting.gofumpt` and `source.organizeImports` are a pair, matching
the `gofumpt` and `goimports` formatters enabled in `.golangci.yml`: with only
the first, a format-on-save file still fails `task fmt` on import
grouping.

The `docker-outside-of-docker` Feature is what makes this usable for the kind
stack rather than a generic Go box: it puts the host docker socket in reach, so
`kind` creates cluster nodes as siblings on the host daemon. The Feature ref is
pinned; the docker CLI it installs is not, because the failure that actually
bites is a CLI older than the host daemon.

One thing that Feature does not solve, and the reason `task kind:up` is a host
command. `kind create cluster` writes a kubeconfig pointing at
`127.0.0.1:<port>` — the *host's* loopback, not the container's — and `kind:up`
creates the cluster and runs `kubectl apply -k infrastructure/local/observability` in one
shell block, so there is nowhere to interleave the repointing below. Run from
inside the container it fails either way: on a fresh machine the apply is
refused at the host loopback, and if the cluster already exists the container
sees the host daemon through the mounted socket, `kind get clusters` skips
creation, and the apply dies on a context that was never written.

From inside the container you attach to a cluster the host already created:

```bash
mkdir -p ~/.kube
kind get kubeconfig --name go-agents > ~/.kube/config
docker network connect kind "$(hostname)"
kubectl config set-cluster kind-go-agents \
  --server=https://go-agents-control-plane:6443
```

`kind get kubeconfig` reaches the host daemon over the mounted socket, so it
works before the container is on the `kind` network; the network connect is
what makes the control-plane name resolve, and the apiserver certificate
already carries it as a SAN. Nothing else needs repointing: every `kind:*` task
passes `--context kind-go-agents` explicitly.

Ports 8080 (API) and 9090 (admin: `/metrics`, `/debug/pprof`) are forwarded.
`postCreateCommand` runs `go mod download` and nothing else. Installing the git
hooks is deliberately left to the host: `.git/` is inside the bind mount, and
`uvx pre-commit install` writes an absolute interpreter path into the shared
`.git/hooks/pre-commit`, so a container-side install breaks `git commit` on the
host and a host-side install breaks it in the container — whichever ran last
wins. Pointing `core.hooksPath` at a container-only directory is no escape,
since `.git/config` is in the same mount. Run `task hooks:install` once on the
host; inside the container, run the gates through `task check`.
