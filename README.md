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

## Report `$ctx` from Claude's statusline

Each Claude agent pushes its own context use to its pane as the `ctx` token:
a bare integer from 0 to 100, source `agentisan`, with a 180 s TTL. After
`task install` puts `herdr-agentisan` on your `PATH`, add one line to the
statusline script (`~/.claude/statusline.sh`), after it has read stdin into
`$input`:

```bash
herdr-agentisan report statusline <<<"$input" >/dev/null 2>&1 &
```

It prints nothing and always exits 0. Outside a herdr pane, or when the
statusline has no context percentage yet, it reports nothing. Set
`HERDR_AGENTISAN_LOG_LEVEL=debug` and drop the redirect to see why.

## Report `$ctx` from Codex

Each Codex agent pushes the same `ctx` token through two Codex hooks, with the
number Codex itself shows as "Context N% used". Add them to
`~/.codex/config.toml`, or to a trusted project's `.codex/config.toml`
([sample](docs/config/codex-hooks.example.toml)), and trust them once when
Codex asks:

```toml
[[hooks.PostToolUse]]
matcher = "*"

[[hooks.PostToolUse.hooks]]
type = "command"
command = "herdr-agentisan report codex"
timeout = 10
async = true

[[hooks.Stop]]

[[hooks.Stop.hooks]]
type = "command"
command = "herdr-agentisan report codex"
timeout = 10
async = true
```

Start Codex with `codex --no-daemon`. By default Codex runs its sessions in a
shared background server whose hooks carry the environment of whichever pane
started it, so the report cannot tell which pane it belongs to and reports
nothing. Like `report statusline`, it prints nothing and always exits 0; the
token expires 180 s after the last tool call or turn, so an idle Codex agent's
`ctx` goes blank. `HERDR_AGENTISAN_LOG_LEVEL=debug` logs why nothing was
reported. ADR-001 A22 has the details.

## Report `$item` and `$stage` from agentisan

Agentisan runs `report stage` on every pipeline step transition once
`herdr-agentisan` is on your `PATH`; there is nothing to wire up:

```bash
herdr-agentisan report stage --item=NERD-5253 --stage=build_test
```

It sets the pane's `item` and `stage` tokens, source `agentisan`, with a 24 h
TTL renewed by every report; they also leave with the pane. A value is at
most 80 printable characters, and anything else is refused rather than cut
short. Like `report statusline`, it prints nothing and exits 0 whether or not
it reported; only a malformed command line (an unknown flag, an extra
argument) fails.

Run by Codex (it carries `CODEX_THREAD_ID`), `report stage` reports only from
the pane's own Codex started as `codex --no-daemon --sandbox danger-full-access`.
Under the shared app-server daemon, with Codex's default sandbox (even for an
approved escalation), or off Linux, it reports nothing (ADR-001 A25).

## Search on prefix+/ and in the dashboard

prefix+/ opens herdr's own Go To, which lists every workspace and pane: press
`/` there and type, and it matches agents by name and spaces by label; Enter
jumps. Bind it beside herdr's default prefix+g by adding the `goto` line to
the `[keys]` table of `~/.config/herdr/config.toml`:

```toml
[keys]
goto = ["prefix+g", "prefix+/"]
```

Inside the dashboard, `/` narrows the agents as you type. Each word of the
query matches letters in order, gaps allowed, across an agent's name, group,
item and stage, so `cod 5255` finds the coder on NERD-5255. The arrows move
among the matches, Enter jumps and zooms, and Esc clears the query.

## Back on prefix+b

The `back` action returns to the pane you were on before, un-zooming the one
you leave. Pressed again it keeps walking back, like a browser's: after jumps
to A, B, C and D, Back goes to C, B, then A; any other focus change ends the
walk. The daemon picks the pane from its focus history; when Back cannot go
anywhere (no daemon, nothing older), a toast says why. Plugins cannot ship
key bindings, so bind it in `~/.config/herdr/config.toml`, in place of the
`pkill` workaround, and reload herdr's config:

```toml
[[keys.command]]
key = "prefix+b"
type = "plugin_action"
command = "nerdsrun.agentisan.back"
description = "Agentisan: back to the previous pane"
```

This takes prefix+b from herdr's default `toggle_sidebar`; see
[`docs/config/herdr-keys.example.toml`](docs/config/herdr-keys.example.toml).

## Gates

`task check` runs most of what CI runs: lint, format, vet, gosec, gitleaks,
govulncheck, race-enabled unit tests, the coverage floor, the large-file
ceiling, the docs parity check, and the pre-commit hooks. `go mod tidy` and
`go tool modernize` are separate CI jobs (`task tidy`, `task modernize`) not included in `check` — see
the command table in [AGENTS.md](AGENTS.md#commands) for the full, CI-verified
list.

Two of those have no Go builtin and are implemented in `internal/devctl`:

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
