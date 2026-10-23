# AGENTS.md

A herdr plugin written in Go (`herdr-agentisan`) and its development tooling,
structured so autonomous coding agents can change it safely. Every rule below
is enforced by a gate — if you break one, CI blocks the merge.

## Bootstrap

```bash
go mod download && task build
uvx pre-commit install   # once per clone; wires the git pre-commit hook
```

That is the whole bootstrap. Tool dependencies (golangci-lint, gosec,
govulncheck, modernize) are pinned in `go.mod` under the `tool` directive and
resolve through `go tool` — there is nothing to install globally and no
version to keep in sync by hand. `task tools` lists what is pinned.

Requires Go 1.26.10+ (pinned by the `toolchain` directive in `go.mod`; earlier
1.26 patches carry reachable stdlib advisories that `govulncheck` fails on,
most recently GO-2026-6809 in `crypto/x509`, fixed in 1.26.10)
and [Task](https://taskfile.dev). `task --list` shows
everything.

## Commands

| Task | Command |
|------|---------|
| Lint | `go tool golangci-lint run --allow-serial-runners ./...` |
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
| Link the plugin into the running herdr | `task plugin:link` |
| Unlink the plugin from herdr | `task plugin:unlink` |
| Run the events' collector into Elasticsearch | `task otelcol:up` |
| Stop the events' collector | `task otelcol:down` |
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
`internal/devctl/parity.go`. A command table that drifts from CI teaches the
next agent to "fix" a gate that was never broken.

The last rows are local-only: CI never installs the git hook, runs the
interactive CLI, links into a herdr (it has none), runs the events'
collector (it has no Elasticsearch), or installs into a developer's `GOBIN`
— and the tdaddy rows need a binary CI cannot install (see below).

Two maps in `internal/devctl/parity.go` are escape hatches from this check:
`localOnly` (documented commands CI need not run) and `nonGateTasks` (tasks not
compared against CI at all). **Adding an entry to either weakens the gate.**
Every entry carries a stated reason; if a parity failure tempts you to add a
name, the fix is almost always to make CI and the docs agree instead. The
same check also fails on an entry with nothing behind it — a task or table row
that was deleted while its exemption stayed — because a dead entry silently
exempts whatever is later added under that name.

## Reporting (in a herdr team)

When you work in a herdr-agentisan team, the dashboard and the sidebar show
your item and stage, read from your own words by the turn hooks
(`herdr-agentisan report turn`, ADR-001 A42). Nothing else reads them, and a
turn that states neither reports nothing, so your stage ages. Claude and Codex
follow the same rule.

- **Stage:** end the final message of every turn with a line that opens with
  `BOSS:` (you report to a boss) or `STATUS:` (no boss above you), in
  capitals, followed by what you are doing or what you need. It must be the
  very last line of the message, with nothing after it. A status line
  anywhere else, inside a sentence or behind a list marker, is not read. If
  you open a message with a status headline, repeat it as the last line.
  Where your role names its own closing lines, as `roles/review.md` does,
  write those instead: they keep this form.
- **Item:** whenever your task changes, state it on a line that opens with
  `item:`, such as `item: NERD-5370` or `item: PR#100`, directly above the
  status line. One inside a sentence is not read.

```text
item: PR#100
BOSS: #100 fix pushed 273f69f; QA re-checks
```

## Layout

- `herdr-plugin.toml` — the plugin manifest herdr reads. Every action execs
  `bin/herdr-agentisan <subcommand>`; `TestManifestMatchesTheBinary` in
  `internal/cli` fails the build if an action stops naming a real command or
  the build output path drifts from the Taskfile's. Its `[[panes]]`: `btop`,
  the popup the dashboard's `[ btop ]` button asks herdr to open
  (`plugin.pane.open`; herdr runs it, the plugin starts no process,
  `TestManifestDeclaresTheBtopPopup`), and `dashboard`, the split that runs
  the dashboard against the daemon (`TestManifestDeclaresTheDashboardSplit`).
  The `back` action's prefix+b binding, Go To's prefix+/ (ADR-001 A20) and
  the dashboard's prefix+d (A18), for herdr's own config, are
  `docs/config/herdr-keys.example.toml` (`TestKeySnippetBindsBack`,
  `TestKeySnippetBindsGoTo`, `TestKeySnippetOpensTheDashboard`).
- `cmd/herdr-agentisan/` — the plugin binary's entrypoint. Wires config,
  logging (stderr only — stdout is the command's output) and tracing; no logic.
- `cmd/devctl/` — development operations CLI entrypoint. A thin shim.
- `internal/cli/` — the `herdr-agentisan` cobra tree. Parses, calls a domain
  package, renders. Domain packages live beside it under `internal/`, one per
  concern; no command holds a rule of its own.
- `internal/plugin/` — the plugin's domain: the runtime environment herdr
  injects (`EnvFrom`), the manifest (`LoadManifest`, with its actions and
  panes) and the actions' logic, such as Back's toast when it went nowhere.
  Also the state directory a command finds the daemon under: herdr's own
  for the plugin, worked out as herdr 0.9.5 does, when herdr set none, as
  in a pane shell (`StateDir`, ADR-001 A51).
- `internal/report/` — an agent pushing its own state to its pane as herdr
  tokens (ADR-001 D4), and the token contract for the keys it writes.
  `report statusline` turns Claude's statusline JSON into `ctx`, with its
  `ctx_at` (A61), and the `model` it runs, with its `model_at` (A56),
  `report codex` does the same from a Codex hook and its session's rollout
  (A22, hooks sampled in `docs/config/codex-hooks.example.toml`), with a
  24 h TTL (A27), and
  `report stage` pushes `item` and `stage` on each agentisan step
  transition, and `report turn` pushes them from the `item:` and
  `BOSS:`/`STATUS:` lines of a Claude or Codex turn, each stage with its
  `stage_at` (A42, hooks sampled in
  `docs/config/claude-hooks.example.json` and
  `docs/config/codex-turn-hooks.example.toml`). They read `HERDR_PANE_ID`, a
  pane-shell variable, which is why it is not in `plugin.Env`.
- `internal/codexhooks/` — installing the `ctx` Codex hooks (ADR-001 A43):
  `codex hooks` writes them into `$CODEX_HOME/hooks.json`, after every entry
  already there, which it keeps as written, and never edits `config.toml` or
  trusts a hook; `--check` only reports. `--turn` adds A42's two
  `report turn` hooks, each in a group of its own. Tests use temp Codex
  homes, never the real `~/.codex`.
- `internal/daemon/` — the daemon's lifecycle (ADR-001 D3, A3, A4): one daemon
  per herdr server, enforced by a flock'd `daemon.lock` that records the
  server's socket inode; the protocol pin; `daemon.log` capped at 10 MB; the
  server's `state.db`, which it holds open while it runs and records every
  `pane.focused` event in (resubscribing and resyncing on `events_lost`); and
  the plugin socket (D6) that answers `health`, `back` (A16: walk the
  focus history, un-zoom the pane left, then focus the target), `snapshot`
  and the workforce ops (A23, `workforce.go`), each with a budget of its
  own, which their client gives every ask, a not_ready one asked again for
  10 s (A52). Every poll it also reconciles `state.db`'s `workers` rows with
  herdr's panes (`bind.go`, A35), rebuilds the team model, pushes `$team`
  and every agent pane's ctx display (`team.go`, A28) and caches every team's
  snapshot, each row with the model its agent reports, while the report is
  the current agent's (`team.CurrentModels`, A56), else its launch model from
  the pane's processes
  (`models.go`, A39; one `pane.process_info` reply per pane per poll, shared
  with the reconcile, `processinfo.go`), and its ctx only while the current
  agent reported it, gated by those processes' starts before anything reads
  it, team status included (`team.GateCtx`, A61), and the boss with its
  agent process's start, its runtime (A47), with the workspaces each holds,
  which the `snapshot` op serves by project or workspace, so the op never
  calls herdr (A18, A24,
  `snapshot.go`, with `SnapshotSource`, the dashboard's client), each group
  with its description, re-read when the shared file changes
  (`descriptions.go`, A36), and sends each status change since the last
  poll as a workflow event (`events.go`), and tells each team's boss what
  the poll sees, through `internal/notice` and the inbox (`notices.go`,
  A44), with the context levels each agent pane crosses and, at the
  ceiling, a toast for the user (A33), less the workers the boss muted
  with the `notices.*` ops (`noticeops.go`). It serves each team's board
  to its boss and the user's dashboard alone (`boardops.go`, A60), each
  caller placed by the kernel's peer credentials (`peer_linux.go`,
  `peer_darwin.go`) and its process ancestry. It keeps
  herdr's agent view on the focused team, which focus events and the poll
  feed it, and clears it outside every team and on a clean stop (`view.go`,
  the A28 addendum, A49).
  It types each queued message into its recipient with `agent.prompt`, by
  the agent name the poll's last model shows where it belongs, or a pane
  sender's by the pane showing its terminal, returns an ended binding's
  queue to its boss, and fails, never retrying, whatever a timeout or a
  crash may have typed (`messages.go`, A2, A30). The `send` op queues them
  (`docs/messaging.md`).
  `daemon start` spawns a detached `daemon run` and waits for its health or
  its exit (A48); herdr's `[[startup]]` hook runs it, and the
  `daemon-restart` action covers linking. Tests run daemons in process
  against herdrtest and a scratch state dir, never the real ones.
- `internal/team/` — the team domain model (ADR-001 D5, A17): project → group
  → logical worker → current pane, built from `pane.list`, `workspace.list`,
  `tab.list`, `agent.list` and `state.db`'s `workers` rows, joined by
  terminal, with the poll's reconcile plan (`bind.go`, A35). A `Resolver`
  maps the layout onto it: `Spaces` for the ◆ spaces, with v12's group
  order, and `Tabs` for a team's one workspace and one tab per worker (A19).
  Also the `$team` format and the ctx display's band tokens (A28), with the
  sidebar rows that show them, `docs/config/herdr-sidebar.example.toml`, for
  herdr's own config (`TestSidebarSnippet`), and the model as a snapshot v1
  in the resolver's group order, with the in-memory stage clock (A18) and
  idle clock (A23), whose working stretches tell an old stage from a
  current one (`stage.go`, A42: `StageOf`, and the sidebar's `stage_now` and
  `stage_old`).
- `internal/notice/` — boss notices (ADR-001 A44): the team poll's source
  (`Watch`: a turn's end from `agent.list`'s `completion_seq`, blocked and
  its end, unknown for two polls (a new agent's for 20), gone (a pane closed
  or moved, or its agent exited), and the daemon's start), the context
  levels' source (`CtxWatch`: each
  agent pane's ctx crossing its rotation thresholds, the boss's included,
  remembered by terminal and re-armed 5 under, A33), GitHub's events
  (`github.go`, A45: merged, CI failed or passed, a conflict, a 👍, a
  review with its P-badge counts, each with its seen-set key), the
  per-boss pacing (`Batcher`: coalesced, at most
  one message per gap, repeats and muted workers dropped, 8 KiB a message),
  the boss's mutes (`Mutes`) and the `Sink` that delivers it: the daemon's
  appends each message to the boss's notice still queued in the inbox
  (`AppendTo`, `store.Notify`), and logs it to daemon.log. Pure: tests
  build models by hand.
- `internal/workforce/` — the boss's team commands (ADR-001 D11, A23),
  `team status`, `team up`, `worker spawn` and `worker retire`, and `send`'s
  sender and recipient (A30): each group's counts and members on a tabs team
  or the ◆ spaces, with idle times from the daemon's clock and the rows no
  pane is bound to (A35); opening a tab per worker and waiting for its agent,
  a Claude one launched with A42's turn hooks as `--settings`,
  after `internal/preflight`'s check and, for a coder with a task packet,
  Jev's answer (`jev.go`, A57), each group up to its count for
  `team up`, which also opens the team's dashboard above the boss
  (`dashboard.go`); closing the pane a worker's row is bound to, after its
  handoff when asked, with the row retired, never while its handoff is
  pending, and failing the handoff whose replacement pane it closes (A33);
  and `handoff ready` (`handoff.go`, A33), the daemon writing the `handoff`
  token on a worker's agent pane once `pane.get` proves its terminal, with no
  row changed, and `handoff ack` (`ack.go`), completing a rotation once its
  replacement reports the old pane's item and stage (I2), the herdr name
  moved first, each step reported (`HandoffSteps`). The daemon runs them,
  with the plugin's dashboard pane and the Codex hooks check `internal/cli`
  gives it; `internal/cli` only asks it, and asks herdr only to confirm its
  own pane (`FindCallerPane`), when no `--project` names the team, for
  `send`'s sender and for `handoff ready` and `handoff ack` with no target;
  under Codex, only from the pane's own Codex, else no pane and no
  `HERDR_AGENTISAN_WORKER` (`FindCaller`, `caller_unverified`, A64).
  Tests drive a fake herdr (`fake_test.go`) and a fresh state.db.
- `internal/settings/` — the versioned `herdr-agentisan.toml` contract (ADR-001
  D9, D12) behind `config resolve`: layered precedence with provenance, secret
  references (`{ env }`, `{ op = "op://…" }`) and the Agentisan launch check,
  each group's one-line description (A36), never from a repo file, and each
  project's test-slot lock files, which the daemon reads once at start
  (`tests.go`, A40), and every project's rotation thresholds, a group's
  own included, which the daemon reads once at start, the repo file
  included (`rotation.go`, A33). A repo file is committed, so it sets only
  colour, github, group counts, models and rotation (A46, `repoFileKeys`,
  refused by default, an empty table adding an entry included, A53), and
  the merged model, whichever layer sets it, is checked as safe to pass to
  an agent
  (`CheckModel`), and the sandbox each worker type's Codex starts in when
  its command chooses none (`sandbox.go`, A59: read-only for review, full
  access for coder, qa and precheck), which spawn adds. Each type runs
  claude or codex, its default provider, with a command and a model per
  provider (`[agent.<provider>]`, `AgentFor(type, provider)`, flags only,
  A62); nothing chooses the other provider yet (NERD-5519). The debugger
  refuses codex and review claude, as gaps, and a model whose name says the
  other provider runs it is refused by `config resolve --project`
  (`CheckModelProviders`) and at spawn. Sample in
  `docs/config/herdr-agentisan.example.toml`.
- `internal/routing/` — the model and effort each spawned worker starts
  with (ADR-001 A50, NERD-5289). It reads the user's routing rules,
  embedded byte for byte from `rules/jev-routing-v1.json` and decoded
  strictly, and the deployment facts from `[routing]` (`internal/settings`).
  `Policy.Resolve` maps a worker type's role and provider, the boss's task
  packet (`DecodeTask`, bounded) and overrides onto a binding, the user's
  override binding only workers of its own provider, one binding per
  provider (`RoleOverride`, A63). It returns a
  `Decision` that is also the audit record. `worker spawn` asks it once per
  spawn, and `team up` once per group it fills, before anything opens; each
  records it on each worker's row and sends it as an
  `agentisan.worker.routed` event. A coder's decision says when Jev should
  be asked (`AsksJev`), and `ApplyJev` maps Jev's answer onto it (A57): the
  request is `JevRequest`'s, and the caller makes the call, so routing never
  sees the key; `[routing.jev]` switches it (`JevEnabled`) and sets its
  thresholds. `worker spawn` asks Jev (`internal/workforce/jev.go`), through
  the daemon's `jev.Asker`; `team up` never does.
- `internal/jev/` — the client of TypeSafe's System One (ADR-001 A57): one
  POST of the body routing built, with the key as the Bearer token, within
  the rules' timeout, never retried or redirected; no error carries the key,
  the request or TypeSafe's body. `Keys` reads the key, `TYPESAFE_API_KEY`,
  from `[routing.jev] env_file`: kept 10 minutes, one read of a file in
  flight, each ask waiting at most a second, so a locked 1Password costs a
  spawn a second; `Asker` drops a key TypeSafe refuses. The daemon holds
  one `Keys` for its life. Tests run an
  httptest TypeSafe, never the real one, and a FIFO for a locked file.
- `internal/store/` — `state.db` (ADR-001 D13, A1): SQLite through the pure-Go
  `modernc.org/sqlite`, WAL with `synchronous=FULL`, and numbered migrations
  embedded from `migrations/` and tracked by `user_version`. The daemon holds
  the only write connection. `state.db` also holds messaging's inbox
  (`messages.go`, migration 0002, D8 and I3): a recipient's messages are
  claimed one at a time, oldest first; the daemon's notices to a boss are
  added to the one still queued (`notify.go`, A44); and a pending handoff
  (`handoffs.go`) holds a worker's inbox until it completes, which rebinds
  the worker in the same transaction, onto the terminal the ack proved, or
  fails; while it is pending no ending is written to the worker's row, and
  a worker still starting begins none (A33). A `workers` row binds its
  pane by terminal, with the shell the team poll read (migration 0003,
  A35), and the routing decision
  it was launched with (migration 0005, A50); the poll's
  writes are compare-and-sets on the row as it read it. The GitHub notices'
  seen-set, open PRs and seed marks, a kind's own included (`github.go`,
  migrations 0004 and 0006, A45), make each event told once across
  restarts. Each team's board (`board.go`, migration 0007, A60) holds its
  posts and two read marks, the boss's and the user's; every post wakes
  each `board wait`. Tests open a fresh database in a temp
  dir; the store is never mocked.
  `boardnotice.go` also provides the pure version-1 GitHub capture-ID helper
  and its frozen envelope/receipt types (B0a.1), with no production callers
  or storage/effect API; durable notice admission remains incomplete.
- `internal/herdr/` — the herdr socket client (newline-delimited JSON, one
  request per connection). `herdrtest` is an in-process fake server: tests
  must dial it, never `HERDR_SOCKET_PATH` — a shell inside herdr has the real
  socket set, and a test that reads it would toast a live session.
- `internal/github/` — the dashboard header's GitHub reads (ADR-001 A29):
  `gh pr list` and a GraphQL open-issue count, run through the gh CLI with an
  explicit path, a timeout and a process group of its own, and mapped onto
  the snapshot's PR states. `ghtest` is a checked-in stand-in gh: tests run
  it with an environment they build, never the real gh or GitHub. The
  daemon's collector (`internal/daemon/github.go`) runs it every 60 s beside
  the team poll and caches each project's result for the snapshot, then
  diffs each list against state.db and hands the boss's GitHub notices to
  the poll (`ghnotices.go`, A45), looking up a PR that left the list with
  `gh pr view`, and a new review's P-badge counts with one GraphQL call
  (`reviews.go`).
- `internal/testslots/` — the dashboard header's test slots (ADR-001 A31,
  A40): which of a project's lock files some process holds a lock on, read
  from the kernel's `/proc/locks` once every team poll without opening or
  locking a lock file, three times with staggered pages, since a page can
  skip a line while other locks change, and which of those a boss's
  `flock <lock> sleep …` holds, told by the holder's `/proc` cmdline and
  open files. Tests hold real locks on temp files, and run a real flock(1)
  hold, and read them back; the parser and the hold check are tested on
  their own against written lines and a stand-in `/proc`, and a
  page-at-a-time stand-in (`seqfile_test.go`) forces the skip.
- `internal/snapshot/` — the versioned snapshot contract (ADR-001 D5/D6,
  A18): the groups and agents, each with the agent herdr detects in its
  pane (A68), the boss, and the project's PRs, issues and test slots.
  Decoding is strict: exact keys, closed enums, RFC 3339 times.
  The daemon's `snapshot` op serves it; `FileSource` reads a fixture.
- `internal/dashboard/` — the bubbletea team dashboard and its pure renderer:
  a header box, then the groups stacked as boxes with one dense line per
  agent, in the snapshot's group order, scrolling to keep the selection in
  view. It reads the daemon (`DaemonConfig`) or a fixture, and Jump focuses
  an unnamed row by its pane id. From the daemon it shows its own team:
  `locate.go` finds the pane running it by its pid, and the daemon maps that
  pane's workspace to a team (A24). `/` narrows the agents to a search query in
  place (`search.go`, ADR-001 A20), and 1-9 or a click on a group's title
  opens the group's view, a box per agent from the snapshot alone
  (`groupview.go`, A37). Also the team's one line for herdr's tab
  bar (`tabbar.go`, A41), which `status line` prints, with its
  `tab_bar_right` entry `docs/config/herdr-tab-bar.example.toml`, for herdr's
  own config (`TestTabBarSnippet` in `internal/cli` runs it). Goldens in
  `testdata/` (`go test ./internal/dashboard/ -update`, then read every one).
- `internal/devctl/` — the `devctl` cobra tree: the coverage floor, the
  docs-parity check and the large-file ceiling, none of which the Go toolchain
  provides.
- `internal/preflight/` — the D12 spawn precondition (ADR-001 I8, A10, A34):
  in a repo with a `go.mod`, `tdaddy` must run from the daemon's PATH and the
  Taskfile must have `tdaddy:index` and `tdaddy:impact`; each failure is a
  sentinel whose message says how to fix it. Tests inject PATH, files and the
  runner.
- `internal/config/` — environment-backed config. No secret has a default.
  Also carries build provenance (`Version`, `Commit`, injected at link time):
  it is configuration resolved from the environment the binary was BUILT in,
  and keeping it in a package of its own made every caller import two.
- `internal/logging/` — zerolog construction and context plumbing.
- `internal/testtmp/` — `Use`, from TestMain: test state on tmpfs, and
  `CheckUsed`, one test per caller that fails when its TestMain stops calling
  `Use` (NERD-5503).
- `internal/telemetry/` — OpenTelemetry tracing over OTLP/HTTP, and the
  daemon's workflow events (ADR-001 D10, A21) as OTLP log records, schema in
  `docs/telemetry.md`. With no endpoint configured it installs propagation
  only and exports nothing. `telemetrytest` records events in memory: tests
  assert on it and never on a real collector. The collector that carries the
  events into Elasticsearch is `docs/config/otelcol-agentisan.yaml`, run by
  `task otelcol:up`; `collector_test.go` pins its keys and the task's
  `docker run`.
- `internal/trail/` — the trail read back (ADR-001 item 12, A32): an ES|QL client
  over `logs-agentisan.otel-*`, the debugger's recipes, and the env file its
  URL and read key come from, `[debugger] env_file` in the shared config,
  read at each query. No caller chooses the URL, the variables or the file.
  Its reader, `ReadEnvFile`, also reads Jev's key for `internal/jev`.
  Tests run an httptest Elasticsearch on a recorded answer
  (`testdata/worker-trail.json`).
- `internal/debugger/` and `roles/debugger.md` — the debugger agent that
  ships with the plugin (item 12, A32): its role, and its launch as data,
  claude with `--permission-mode dontAsk`, a read-only allow list and its
  reads fenced to the repo it starts in and the daemon's directory.
  `herdr-agentisan events` and `debugger args` are its commands, and
  `worker spawn --group debugger` starts it as a team worker (A32 part 2),
  the launch's inputs looked up by `Root`, `CheckRoot` and `KeyFiles` for
  both; `TestRoleMatchesTheLaunch` keeps the role, the CLI and the launch in
  step.
- `roles/review.md` — the Codex review worker's role (ADR-001 A55):
  agentisan-skills' `## Review guidelines`, vendored byte for byte, with the
  reviewer's read-only rules, its report's fields and its final line. The
  boss sends it whole with `send` before each review; no Go reads it.
  `internal/settings/reviewrole_test.go` pins the section's hash and commit,
  the guards and the read-only default, and
  `internal/routing/reviewrole_test.go` routing's `findings_require`.
- `.devcontainer/` — the reproducible dev environment: Go 1.26.10 plus the
  two tools `go tool` does not resolve (Task, uv).
- `docs/architecture.md` — why the pieces are shaped this way, and the
  deliberate limits.
- `docs/adr/` — accepted architecture decisions. `0001-boss-led-workflow-plugin.md`
  (ADR-001; its amendments are in `docs/adr/0001/`, one file each: read them
  all) is the design source of truth for the daemon, token contract, storage
  and build order; a change that contradicts it needs a superseding ADR, not a
  quiet edit.

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
Measured in this repo: one impact query takes ~12s, while the entire test
suite (84 tests) runs in ~0.7s (~2.2s with `-race`). Asking which tests to
run costs about 15x more than running all of them, so an edit-loop hook is
pure tax here. The hooks tdaddy can install (`tdaddy hook claude|codex
install`) were tried and deliberately removed.

Selectivity is weak on widely imported files: `config.go` flags 41 of 84
tests (~49%), which does not narrow much. Leaf files narrow better
(`coverage.go` -> 23 of 84, ~27%; `parity.go` -> 25 of 84, ~30%). These
counts will keep moving as the suite grows; re-measure with `tdaddy impact
--files <path> --max-tests 0` rather than trusting the numbers here
indefinitely.

tdaddy earns its keep on repositories whose suites take minutes; its cost is
roughly constant (BadgerDB open dominates), so the ratio is what decides. Use
it here for the occasional "what does this actually touch?" question --
especially the cross-package answer -- and run `task check` for everything else.

**Its answer is advisory, never the gate.** `task check` is the gate,
whatever tdaddy says a change affects. Having tdaddy set up is not optional,
though: ADR-001 D12 and I8 require it for every worker in a Go repo, and
`internal/preflight` checks it (`tdaddy --version` runs from the daemon's
PATH, and the Taskfile has `tdaddy:index` and `tdaddy:impact`).

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
3. **Business logic goes in a domain package under `internal/`, never in a
   command.** A rule implemented in a cobra command is a rule no other caller
   has — not a herdr event hook, not a test. Commands parse, delegate and
   render.
4. **Return wrapped errors; map them with `errors.Is`.** Domain code returns
   exported sentinel errors wrapped with `%w`. Callers branch on those
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
