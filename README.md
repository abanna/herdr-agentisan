# herdr-agentisan

An Agentisan plugin for [herdr](https://herdr.dev), written in Go, plus the
development tooling that keeps it safe for autonomous coding agents to change.
Two binaries:

| Binary | Purpose |
|--------|---------|
| `herdr-agentisan` | The plugin binary herdr invokes from its manifest |
| `devctl` | Development operations: coverage floor, docs parity, large files |

herdr runs the plugin from `herdr-plugin.toml`; see
[herdr's plugin docs](https://herdr.dev/docs/plugins/) for the model.

## Quick start

```bash
go mod download && task build

task plugin:link                                   # build, then link into the running herdr
herdr plugin action invoke nerdsrun.agentisan.ping # a toast appears
herdr plugin log list --plugin nerdsrun.agentisan  # exit 0, "notification shown"
task plugin:unlink
```

`herdr plugin link` never builds, which is why `task plugin:link` does. The
binary also runs by hand: `task run:cli -- version --json`.

Requires Go 1.26.9+ and [Task](https://taskfile.dev). Linters and scanners are
pinned in `go.mod` under the `tool` directive — nothing to install globally.

## Gates

`task check` runs most of what CI runs: lint, format, vet, gosec, gitleaks,
govulncheck, race-enabled unit tests, the coverage floor, the large-file
ceiling, the docs parity check, and the pre-commit hooks. `go mod tidy` and
`go tool modernize` are separate CI jobs (`task tidy`, `task modernize`) not included in `check` — see
the command table in [AGENTS.md](AGENTS.md#commands) for the full, CI-verified
list.

Two of those have no Go builtin and are implemented in `internal/devcli`:

- **Coverage floor** — `go test` has no `--cov-fail-under`, so `devctl
  coverage` parses `go tool cover -func` and fails under 75%.
- **Docs parity** — `devctl docs-parity` proves the AGENTS.md command table,
  `Taskfile.yml` and `.github/workflows/ci.yml` describe the same gates, in
  every direction. A command table that drifts from CI teaches the next agent
  to "fix" a gate that was never broken.

## Agent instructions

`AGENTS.md` is canonical. `CLAUDE.md` is a single `@AGENTS.md` import so
Claude Code picks it up without creating a second source of truth.

## Layout

See the Layout section of [AGENTS.md](AGENTS.md).

## Dev container

`.devcontainer/` builds the local workflow into one image: Go 1.26.9, Task, uv
(which is how `uvx pre-commit` runs).
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

`postCreateCommand` runs `go mod download` and nothing else. Installing the git
hooks is deliberately left to the host: `.git/` is inside the bind mount, and
`uvx pre-commit install` writes an absolute interpreter path into the shared
`.git/hooks/pre-commit`, so a container-side install breaks `git commit` on the
host and a host-side install breaks it in the container — whichever ran last
wins. Pointing `core.hooksPath` at a container-only directory is no escape,
since `.git/config` is in the same mount. Run `task hooks:install` once on the
host; inside the container, run the gates through `task check`.
