# AGENTS.md

Go CLI and its development tooling, structured so autonomous coding agents
can change it safely. Every rule below is enforced by a gate — if you break
one, CI blocks the merge.

## Bootstrap

```bash
go mod download && task build
uvx pre-commit install   # once per clone; wires the git pre-commit hook
```

That is the whole bootstrap. Tool dependencies (golangci-lint, gosec,
govulncheck, modernize) are pinned in `go.mod` under the `tool` directive and
resolve through `go tool` — there is nothing to install globally and no
version to keep in sync by hand. `task tools` lists what is pinned.

Requires Go 1.26.9+ (pinned by the `toolchain` directive in `go.mod`; earlier
1.26 patches carry reachable stdlib advisories that `govulncheck` fails on)
and [Task](https://taskfile.dev). `task --list` shows
everything.

## Commands

| Task | Command |
|------|---------|
| Lint | `go tool golangci-lint run ./...` |
| Format check | `go tool golangci-lint fmt --diff ./...` |
| Vet | `go vet ./...` |
| Run tests with a coverage profile | `go test -race -timeout 5m -coverpkg=./internal/... -coverprofile=coverage.out -covermode=atomic ./...` |
| Enforce the coverage floor | `go run ./cmd/devctl coverage --profile coverage.out --min 75` |
| Static security scan | `go tool gosec -quiet ./...` |
| Secret scan (git history) | `go run github.com/zricethezav/gitleaks/v8@v8.30.1 git . --config .gitleaks.toml --no-banner --redact` |
| Dependency vulnerability check | `go tool govulncheck ./...` |
| Report modernisable constructs | `go tool modernize ./...` |
| Check docs match CI | `go run ./cmd/devctl docs-parity` |
| Check for oversized tracked files | `go run ./cmd/devctl large-files` |
| Run all pre-commit hooks | `uvx pre-commit run --all-files` |
| Tidy dependencies | `go mod tidy` |
| Check go.mod is tidy | `git diff --exit-code go.mod go.sum` |
| Install the git pre-commit hook | `uvx pre-commit install` |
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

The last rows are local-only: CI never installs the git hook, runs the
interactive CLI, or installs into a developer's `GOBIN` — and the tdaddy rows
need a binary CI cannot install (see below).

Two maps in `internal/devcli/parity.go` are escape hatches from this check:
`localOnly` (documented commands CI need not run) and `nonGateTasks` (tasks not
compared against CI at all). **Adding an entry to either weakens the gate.**
Every entry carries a stated reason; if a parity failure tempts you to add a
name, the fix is almost always to make CI and the docs agree instead. The
same check also fails on an entry with nothing behind it — a task or table row
that was deleted while its exemption stayed — because a dead entry silently
exempts whatever is later added under that name.

## Layout

- `cmd/go-agents/` — product CLI entrypoint. A thin shim; no logic.
- `cmd/devctl/` — development operations CLI entrypoint. A thin shim.
- `internal/notes/` — **the domain.** Validation and storage behind `Store`.
  The CLI drives this; no command holds a rule of its own.
- `internal/cli/` — the `go-agents` cobra tree. Parses, calls the domain, renders.
- `internal/devcli/` — the `devctl` cobra tree: the coverage floor, the
  docs-parity check and the large-file ceiling, none of which the Go toolchain
  provides.
- `internal/config/` — environment-backed config. No secret has a default.
  Also carries build provenance (`Version`, `Commit`, injected at link time):
  it is configuration resolved from the environment the binary was BUILT in,
  and keeping it in a package of its own made every caller import two.
- `internal/logging/` — zerolog construction and context plumbing.
- `internal/telemetry/` — OpenTelemetry tracing over OTLP/HTTP. With no
  endpoint configured it installs propagation only and exports nothing.
- `.devcontainer/` — the reproducible dev environment: Go 1.26.9 plus the
  two tools `go tool` does not resolve (Task, uv).
- `docs/architecture.md` — why the pieces are shaped this way, and the
  deliberate limits.

Tests live beside the code they cover, in `package <name>_test`, so they
exercise the exported surface an agent would actually call.

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

Selectivity is weak on the files you edit most: `notes.go` flagged 43 of 144
tests (~30%) when last measured, which does not narrow much. Only leaf
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

1. **Never commit a secret.** Config comes from the environment. No secret has
   a default: when one is added, `config.Load()` must return an error rather
   than fall back. `.env` is gitignored; `.env.example` holds no secret values
   — the OTLP endpoint is left blank, everything else is a safe local default.
2. **New code needs tests.** The floor is 75% total statements over
   `./internal/...`, enforced by `devctl coverage`. The `cmd/` packages are
   outside the denominator on purpose: they are process wiring with no logic
   (rule 3 keeps them that way), and counting them would let real gaps in the
   domain hide behind untestable `main` bodies. Do not lower it, and do not write assertion-free tests to
   clear it — a test that cannot fail is worse than no test.
3. **Business logic goes in `internal/notes/`, never in a command.** A rule
   implemented in a cobra command is a rule no other caller has. Commands
   parse, delegate and render.
4. **Return wrapped errors; map them with `errors.Is`.** Domain code returns
   `ErrNotFound`/`ErrInvalid` wrapped with `%w`. Callers branch on those
   sentinels with `errors.Is` — never by matching on error strings.
   `wrapcheck` enforces the wrapping.
5. **Never edit files on `main`.** Branch first.
6. **Keep changes scoped.** No drive-by refactors of files unrelated to your task.

## Do not touch

- `go.sum` — regenerate with `go mod tidy`, never hand-edit.
- `.github/workflows/` — CI definitions. Changing a gate is a human decision.
- `.golangci.yml` — the enabled linter set is deliberate. Silence a specific
  finding with a `//nolint:<linter>` comment *and a reason*, never by removing
  a linter.
- `.claude/` and `.mcp.json` — the agentisan bundle, installed out-of-band and
  gitignored. Never `git add .` here.
