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

herdr's startup hook runs `daemon start`, and the `daemon-restart` action
`daemon restart`. Each waits for the daemon to answer health, for up to 13 s,
and then says one of three things (ADR-001 A48):

- `daemon running (pid N)`;
- `daemon starting (pid N) but not answering health yet`, exit 0, since it
  may still come up;
- an error, exit non-zero, when the daemon exited before it answered.

`herdr plugin log list --plugin nerdsrun.agentisan` shows which, and the
`daemon.log` the message names says why.

herdr gives a shell in one of its panes no `HERDR_PLUGIN_STATE_DIR`. From
there the commands that ask a running daemon (`send`, `team status`,
`worker retire`, `back`, `dashboard`, `status line`, `daemon health` and
`daemon stop`) find it under herdr's own state directory for the plugin,
from `XDG_STATE_HOME` or `HOME` as herdr works it out (ADR-001 A51).
`daemon start`, `run` and `restart` never guess: run them from herdr, or
pass `--state-dir`.

Requires Go 1.26.10+ and [Task](https://taskfile.dev). Linters and scanners are
pinned in `go.mod` under the `tool` directive — nothing to install globally.

## Report `$ctx` from Claude's statusline

Each Claude agent pushes its own context use to its pane as the `ctx` token:
a bare integer from 0 to 100, source `agentisan`, with a 180 s TTL. In the
same report it pushes the model it runs, the statusline's `model.id`, as the
`model` token, with `model_at` (ADR-001 A56), so the dashboard's model column
follows a `/model` switch. After
`task install` puts `herdr-agentisan` on your `PATH`, add one line to the
statusline script (`~/.claude/statusline.sh`), after it has read stdin into
`$input`:

```bash
herdr-agentisan report statusline <<<"$input" >/dev/null 2>&1 &
```

It prints nothing and always exits 0. Outside a herdr pane, or when the
statusline has neither a context percentage nor a model yet, it reports
nothing. Set
`HERDR_AGENTISAN_LOG_LEVEL=debug` and drop the redirect to see why.

## Report `$ctx` from Codex

Each Codex agent pushes the same `ctx` token through three Codex hooks, with
the number Codex itself shows as "Context N% used", and the hook's `model` as
the `model` token, as the statusline does. Install them once:

```bash
herdr-agentisan codex hooks
```

It adds them to Codex's hooks file, `~/.codex/hooks.json`
(`$CODEX_HOME/hooks.json`), after any hooks already there, and writes nothing
else. Then trust them once: start `codex --no-daemon` and choose "Trust all
and continue", or trust them in `/hooks`. Codex runs a hook only once you
trust it, and asks again when one changes. Codex also trusts each folder by
its exact path, so it asks once in every repo a Codex worker runs in.
`herdr-agentisan codex hooks --check` says where each hook is, and exits 1
unless all three are installed. To keep them in `config.toml` instead, paste
them at its end ([sample](docs/config/codex-hooks.example.toml)); the command
then refuses to add a second copy:

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

[[hooks.PostCompact]]

[[hooks.PostCompact.hooks]]
type = "command"
command = "herdr-agentisan report codex"
timeout = 10
async = true
```

Start Codex with `codex --no-daemon`. By default Codex runs its sessions in a
shared background server whose hooks carry the environment of whichever pane
started it, so the report cannot tell which pane it belongs to and reports
nothing. Like `report statusline`, it prints nothing and always exits 0.
`HERDR_AGENTISAN_LOG_LEVEL=debug` logs why nothing was reported. Codex runs
its hooks outside its sandbox, so `ctx` works whatever the sandbox.

Codex's context changes only in a turn or a compaction, and those are when
its hooks run, so the token lasts 24 h, set again by every report: an idle
Codex agent keeps its `ctx`. PostCompact reports after a compaction, but may
still read the count from before it; the next turn corrects it. Nothing
clears `ctx` when Codex exits, cleanly or not: the last value stays until the
TTL runs out or the next Codex in the pane reports. The daemon ignores it
meanwhile: the sidebar, the dashboard and `team status` read `ctx` only from
a pane where herdr detects an agent. ADR-001 A22 and A27 have the details.

## Report `$item` and `$stage` from agentisan

Agentisan runs `report stage` on every pipeline step transition once
`herdr-agentisan` is on your `PATH`; there is nothing to wire up:

```bash
herdr-agentisan report stage --item=NERD-5253 --stage=build_test
```

It sets the pane's `item` and `stage` tokens, and `stage_at`, the report's
time, source `agentisan`, with a 24 h TTL renewed by every report; they also
leave with the pane. A value is at most 80 printable characters, and anything
else is refused rather than cut short. Like `report statusline`, it prints
nothing and exits 0 whether or not it reported; only a malformed command line
(an unknown flag, an extra argument) fails.

Run by Codex (it carries `CODEX_THREAD_ID`), `report stage` reports only from
the pane's own Codex started as `codex --no-daemon --sandbox danger-full-access`.
Under the shared app-server daemon, with Codex's default sandbox (even for an
approved escalation), or on an OS other than Linux and macOS, it reports
nothing (ADR-001 A25, A26).

Codex workers run under your own Codex sandbox setting, so by default their
`report stage` commands report nothing: they show `ctx`, and an item or a
stage only from the [turn hooks](#report-item-and-stage-from-every-turn),
which run outside the sandbox. To have a group's Codex workers report each
step too, add `--sandbox danger-full-access` to the agent command they
launch with.
That runs their commands with no sandbox and no reviewer, since Codex's
automatic review sees only escalations, which that mode never makes: a weaker
guard than Claude's auto mode (ADR-001 A43).

## Report `$item` and `$stage` from every turn

Any agent, a boss, QA or research pane included, pushes its own `item` and
`stage` at the edges of its turns, with no command typed by hand.
`report turn` runs as an async `UserPromptSubmit` and `Stop` hook, reads
what the turn's text states, and sets the tokens `report stage` sets:

- **The stage** is the final message's closing `BOSS: <status>` or
  `STATUS: <status>` line, less its prefix: `BOSS: blocked: 1Password agent`
  sets `blocked: 1Password agent`. `BOSS:` is the line a worker ends each
  turn with; `STATUS:` is for a pane with no boss above it.
- **The item** is an `item: <id>` line: the first one in a prompt, as a
  dispatch carries it, or the last one in the final message's closing
  paragraph, beside the status line. A new item stated with no stage clears
  the old stage. An item alone never sets `stage_at`, so it never makes an
  old stage look reported again.

Markdown bold and code ticks around a line are fine; a value over 80
characters is cut to 79 and `…`. A turn that states neither reports nothing,
so a pipeline step's stage stays; so do a subagent's turns, and a Claude turn
you interrupt, which runs no `Stop` hook. Agents write these lines only when
told to, so say it in their instructions (`CLAUDE.md`, `AGENTS.md`), for
example: "End every turn with a last line `BOSS: <what you are doing>`."

For Claude, add the hooks to `.claude/settings.json` or
`~/.claude/settings.json`
([sample](docs/config/claude-hooks.example.json)):

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "herdr-agentisan report turn",
            "timeout": 10,
            "async": true
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "herdr-agentisan report turn",
            "timeout": 10,
            "async": true
          }
        ]
      }
    ]
  }
}
```

For Codex, paste the
[Codex turn hooks](docs/config/codex-turn-hooks.example.toml) at the end of
`~/.codex/config.toml`, or of a trusted project's `.codex/config.toml`, and
trust them once. `herdr-agentisan codex hooks` installs only the `ctx` hooks
above. Each turn hook is a group of its own, so that command never finds one
beside `report codex`. They need `codex --no-daemon`, as `report codex`
does: a hook the shared app-server ran reports nothing. Codex runs hooks
outside its sandbox, so they work under the default one, unlike `report stage`
from a tool command. Both print nothing and always exit 0;
`HERDR_AGENTISAN_LOG_LEVEL=debug` logs why nothing was reported. ADR-001 A42
has the details.

## An old stage ages

A stage stays on its pane for up to a day, so the daemon marks one that no
longer says what the agent is doing: once a turn of the agent's has ended
with no report of it, or herdr no longer detects the agent (ADR-001 A42).
The sidebar then shows it dimmed with its age (`build_test · 3h`), the
dashboard draws it faint with "3h05m ago" in its time column, and
`team status` gives `stage_stale`. A stage reported at the end of each turn
stays current while the pane rests, so a `Stop` hook or a closing
`report stage` keeps it so. It needs no hooks: it works on any agent pane a
team places, from herdr's statuses and the `stage_at` every report writes.
The daemon judges only a turn it saw begin and end, so after a restart a
stage stays current until the agent's next turn. It calls a turn ended at
the second poll that sees the pane at rest, about 3 s on, so a `Stop` hook's
report that lands just after the turn ends is still in time.

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

## `$team`, the ctx display and the focused team in the sidebar

The daemon pushes each `◆ <group>` workspace, and each team's workspace, its
`$team` token every 3 s: `4 agents · 0 working · 1 ready`, with
` · 2 blocked` when any are, where `ready` counts the agents waiting for a
handoff. It pushes every agent pane its context use as well, seven
characters wide (`ctx  9%`, `ctx 56%`; `ctx  --` when the agent reports
none), in one of three tokens by band: `$ctx_ok` up to 55%, `$ctx_warn` above
55% and `$ctx_hot` above 65%, and its stage, in `$stage_now` while it is
current and in `$stage_old`, with its age, once it is
[old](#an-old-stage-ages). They go with source `agentisan` and a 9 s TTL,
so they clear themselves within 9 s of the daemon stopping. `◆ boss` and
`◆ herdr` get no `$team`. `[spaces] project` in `herdr-agentisan.toml` names
the project the spaces belong to; with no config file they belong to one
unnamed project. See ADR-001 A17 and A28.

The agents panel shows the team you are in: the daemon sets herdr's agent
view to its workspaces and any blocked agent, labelled with the project
(`◆ spaces` when `[spaces] project` names none), in the team's order. Outside
every team, and when it stops cleanly, it clears the view (A28 addendum).

herdr plugins cannot ship config: to show them, put the rows in
[`docs/config/herdr-sidebar.example.toml`](docs/config/herdr-sidebar.example.toml)
in herdr's `config.toml`. They replace rules that matched `$team`'s old ⚠ and
⟳ glyphs.

## Workflow events

With `[telemetry] endpoint = "127.0.0.1:4328"` in `herdr-agentisan.toml`, the
daemon sends every agent status change it sees between two polls to that
OTLP/HTTP collector as a log record, `agentisan.worker.status_changed`, for
Elastic. Without it nothing is exported. Run the `daemon-restart` action after
setting it. `task otelcol:up` runs that collector in Docker, writing to the
`logs-agentisan.otel-default` data stream with an API key you give it. The
schema and the setup are [`docs/telemetry.md`](docs/telemetry.md); see
ADR-001 A21.

### Cutover from v12's `$team` push

Each key has one writer (ADR-001 D4). Once a daemon with this push is
running, delete the `report_team_tokens(...)` call at line 231 of
`herdr-dashboard-v12.py` and restart v12. Until then both write `$team` and
the last write wins; after, the daemon's next push, within 3 s, replaces the
value v12 left on each group space. The daemon never writes `◆ herdr`, so
v12's last value there clears itself when its 30 s TTL runs out.

## Boss notices

The daemon tells each team's boss what its team poll sees (ADR-001 A44):
- a worker finished a turn: herdr's `completion_seq` for its terminal changed;
- it turned blocked;
- it turned unknown and still is at the next poll;
- it left the team: its pane closed or moved away, or its agent exited and
  left a shell.

From the GitHub collector (A45), it tells each project's boss when one of
its repo's PRs merged, its CI failed or passed at a head, it has a merge
conflict at a head, or it got its first 👍:

```
herdr-agentisan #50 merged as 1e0fb1b
#50 CI failed at 1e0fb1b (2 of 7 checks)
```

Each is told once, across restarts, from `state.db`. A repo's first
collection only records what is true then. GitHub's lines never carry a
title, branch, body or comment.

It also tells each boss, once, that the daemon started. Turns that end while
the daemon is down are not reported. The boss's own panes and plain shells are
never reported. Each line carries the worker's reported ctx, item and stage:

```
ha-coder-2 finished a turn · ctx 44% · NERD-5322 · verify
```

The daemon only informs; it never acts on a notice. Each team's notices are
coalesced, and its boss gets at most one message every `gap` seconds. The same
worker and kind again within `repeat` seconds is dropped. `[notices]` in
`herdr-agentisan.toml`, or `[projects.<p>.notices]`, sets these and `skip`,
the kinds not told; see the sample. The daemon reads them when it starts.

Until notices go through the inbox (NERD-5256), each message is one line in
`daemon.log`. Follow it with:

```bash
tail -F "$STATE_DIR/daemon.log" | grep --line-buffered '"message":"notice"'
```

`$STATE_DIR` is the plugin's state directory, which herdr passes the daemon
as `HERDR_PLUGIN_STATE_DIR`.

### Rotation notices

The boss is also told when a pane's context crosses one of its rotation
thresholds (ADR-001 D7, A33). A level counts as crossed at its threshold or
above:

```
ha-coder-1 is at 66% context (hard: 65%) · NERD-5261 · verify: rotate it now
```

- **What is covered:** every agent pane in a team, the boss's included. The
  boss is told about its own: `you are at 80% context (ceiling: 75%): hand
  off now; the user has been alerted`.
- **Once per level:** each level is told once. It is told again only after the
  context has fallen to its threshold less 5, or below.
- **Unknown context:** a `ctx` that is absent or expired is unknown, and tells
  nothing.
- **The ceiling:** at the ceiling the daemon also shows the user a toast,
  once, and the boss's line says whether it was shown.

The thresholds are `rotation.thresholds`, by default `[55, 65, 75]`, for the
project and the boss. A role can set its own with
`groups.<type>.rotation.thresholds`. The daemon reads both when it starts,
from the shared file, the project's table and the repo file. `skip` can name
`ctx_soft` and `ctx_hard`, but never `ctx_ceiling`.

## The debugger

A read-only Claude agent that answers "what happened to worker X in the last
hour" from the workflow events (ADR-001 item 12, A32). Its role ships here,
in `roles/debugger.md`. `herdr-agentisan events worker <id>` and `events
query '<ES|QL>'` read the events in Elasticsearch. They take its URL and read
key from the env file `[debugger] env_file` names, at each query, so the key
never enters an agent's environment. `herdr-agentisan debugger args` prints
the claude command that starts it read-only, its reads fenced to the repo
and the daemon's directory; `worker spawn --group debugger`
will launch it. See [`docs/debugger.md`](docs/debugger.md).

## Teams: one workspace, one tab per worker

Every configured project but the `[spaces] project` is a team: one herdr
workspace labelled with the project's name, the boss's tab `<short>-boss`,
and one tab per worker, `<short>-<type>-<n>`, such as `ha-coder-2`. Give each
such project a `short` code in `herdr-agentisan.toml`. The daemon pushes the
team's workspace one `$team` for the whole team, the boss left out, and reads
the teams when it starts: run the `daemon-restart` action after adding one.
See ADR-001 A19.

## The workforce commands

The boss reads its team, spawns workers and retires them with commands the
daemon runs (ADR-001 D11, A23); the `herdr-workforce` skill drives them.
`team up` follows in a later part of NERD-5254.

| Command | What it does |
| --- | --- |
| `team status [--project p]` | Every group's counts and open slots, and each worker's status, ctx, item, stage (and whether it is old) and idle time |
| `worker spawn --group g [--count n] [--model m]` | Adds workers, refused whole past the group's `slots` or when the repo fails the tdaddy preflight |
| `worker retire <id> [--after-handoff]` | Closes a worker's pane, by logical id or pane id; with the flag, once its handoff is written |

Each prints one JSON object and exits 1 when refused. Without `--project`
status and spawn read the team whose workspace the command runs in, else the only
tabs team.
A tabs team's worker is named by its logical id, `<short>-<type>-<n>`, its
tab's label; a pane of the ◆ spaces by its pane id. Idle times start when
the daemon does, and a team added to the configuration since gets them
after a `daemon-restart`. A worker whose row no pane is bound to, such as one
lost after a herdr live handoff, is listed under `unbound` (ADR-001 A35).
Just after the daemon starts, the command waits for its first poll.

Each command resolves the project's profile with its repo's committed
`.herdr-agentisan.toml`. So that file sets only `color`, `github`, a
group's `workers` and `slots`, models and the rotation thresholds, the
project's and a group's (ADR-001 A46, A33). Agent commands, `[spawn]`,
`verify`, `[integrations]` and every other key belong in
`herdr-agentisan.toml` or `--set`. A repo file that sets one
is refused, and names the key. A model, from any layer, is a letter or
digit, then letters, digits and `. _ - : / @ [ ]`, such as `opus[1m]`.

### Model routing

The user's routing rules, built in byte for byte
(`internal/routing/rules/jev-routing-v1.json`), choose the model and
reasoning effort each worker starts with, by its type's role (ADR-001 A50).
`[routing]` in the shared `herdr-agentisan.toml` holds what they leave to
the deployment: `enabled` (off by default, and while it is on no
`agent.model` key may be set), each binding's real `model_id`, efforts and
`enabled`, and `[routing.overrides]`, the user's standing choice for a role
and the only way to a premium binding. `config resolve` shows each key with
its layer, then `routing policy:`, the version a decision records.
`worker spawn` asks routing from NERD-5289 part 1b on; until then the keys
resolve and nothing reads them.

## Messaging

`herdr-agentisan send <to> <body|->` queues a message in the daemon, which
types it into the recipient's agent once, holds it through a worker's
handoff, and tells the sender when a prompt may not have arrived (ADR-001
A30). `boss` is your own team's boss. See [`docs/messaging.md`](docs/messaging.md).

## The dashboard

`herdr-agentisan dashboard` draws a team from the daemon (ADR-001 D5,
A18, A24): the team whose workspace the dashboard's own pane is in, so a
dashboard opened in any team's workspace shows that team, with nothing
configured per team. Once a second it asks the daemon socket for that team's
snapshot, which the daemon answers from the team model its 3 s poll already
builds, so drawing never calls herdr; every 10 seconds the dashboard asks
herdr which pane runs it, two calls once it has found its pane. Outside every
team it draws no board and the footer says so; `--project <name>` shows that
project's team instead. Enter or a click jumps to an agent, or to a shell by
its pane. With no daemon, the footer says `daemon not running` and the
dashboard keeps asking.

herdr runs it as the plugin's `dashboard` pane, a split beside the focused
pane. Plugins cannot ship key bindings, so replace the `dashboard-tab.sh`
binding on prefix+d in herdr's config (`~/.config/herdr/config.toml`) with
this, then reload herdr's config:

```toml
[[keys.command]]
key = "prefix+d"
type = "shell"
command = "herdr plugin pane open --plugin nerdsrun.agentisan --entrypoint dashboard --focus"
description = "Agentisan: open the team dashboard"
```

prefix+d is not one of herdr's default keys. Each press opens a new split,
where `dashboard-tab.sh` jumped to the one it had made, so close a dashboard
with `q`. Add `--direction down` to split below the focused pane instead.
See [`docs/config/herdr-keys.example.toml`](docs/config/herdr-keys.example.toml).

`herdr-agentisan dashboard --fixture docs/dashboard/sample-snapshot.json`
draws a snapshot file instead, outside herdr too. A fixture's time is fixed,
so its footer always reads `data Ns old`.

The dashboard keeps v12's cues (ADR-001 A38). A stage that says READY is
bold green, RED or blocked bold red, and waiting or queued yellow, matched
by whole word in any case. The footer says `data Ns old` once the snapshot
drawn is more than 15 s old, so a stalled daemon never passes for a live
one. A plain shell is drawn as `▸ shell`. An [old stage](#an-old-stage-ages)
takes none of those colours: it is dim, faint and italic, and its time
column says how long ago it was reported ("3h05m ago"), so it reads as old
on a terminal that draws neither.

### Group descriptions

A group's box can carry a one-line purpose under its title, as v12's
`dashboard-desc.json` gave it (ADR-001 A36). Set it in `herdr-agentisan.toml`
under the group's name as the dashboard shows it: a tabs team's worker type,
or a ◆ space's name. A group table may set only a description, and then it
starts no worker:

```toml
[groups.coder]                      # every project's coder group
workers = 3
slots = 5
description = "writes fixes and new code"

[projects.agentisan.groups.coders]  # the ◆ coders space
description = "write fixes + new code · run their own review loop"
```

A description is one line of at most 80 printable characters, and a repo's
`.herdr-agentisan.toml` cannot set one. The daemon reads descriptions again
whenever the file changes, so an edit needs no `daemon-restart`; a file that
does not resolve keeps the last ones, and `daemon.log` says why.

### The model column

Each row shows the model its agent reports it runs (ADR-001 A56): Claude's
statusline `model.id`, or the model a Codex hook names. A `/model` switch
therefore shows at the agent's next report: within a minute for Claude, and
after Codex's next tool use or turn end.

Until an agent has reported, or when its report is older than the agent's
process, as a model left by an earlier agent in the pane is, the row shows
the model the agent was started with. That is Claude's `--model`, or Codex's
`-m` or `--model`, read from the pane's processes through herdr (A39).

The daemon asks herdr again only when a pane's agent restarted or changed,
and once a minute for a pane where it finds no agent process. An agent
started without the flag that has not reported yet shows none.

### A group's view

Press 1-9, or click a group's title, to open that group's view (ADR-001
A37): a box per agent, two to a row from 110 columns, with its model and
ctx, item, the whole stage (two lines, then `…`), how long it has been in
that stage and at rest, and the pane and tab it runs in. Everything in it is
what the agents report and the daemon times; nothing is read from a screen,
so a worker's stage line is what says what it is doing. Enter or a click
jumps to the agent, as on the overview; Esc, ← or `◀ back` returns, and `/`
returns and starts a search.

### PRs and issues in the header

For every project with a `github` repo in `herdr-agentisan.toml`, the daemon
asks GitHub for its open PRs and open-issue count every 60 s, through the gh
CLI and its own login (`gh auth status`), and the header shows them (ADR-001
A29). Each PR gets herdr-status.sh's one mark: `✗` a check failed, `⏳`
checks pending, `✓` every check passed with a 👍 and a clean merge, `…`
waiting for the 👍 or a clean merge, `💬` Codex's findings, `?` a state it
cannot read. With no Codex reviewer on a repo, the 👍 is yours.
release-please's PRs are skipped. gh must be on the PATH herdr starts the
daemon with; without it, `daemon.log` says so and the header shows no PRs.
The daemon reads the repos when it starts, so run the `daemon-restart`
action after changing them.

### Test slots and the boss's runtime in the header

The header shows `slots used/total` for the lock files `[tests] locks`
names in the shared `herdr-agentisan.toml` (ADR-001 A31): a slot is in use
while some process holds a lock on its file, as `/proc/locks` lists them,
counted every poll. A project's own `[projects.<p>.tests] locks` replaces
the shared list for its team (A40). Give absolute paths on tmpfs or ext4;
agentisan's gate keeps one per slot in
`$XDG_RUNTIME_DIR/agentisan/gate.<n>.lock`. A slot a boss holds with
`flock <lock> sleep …` shows as held, not used: `slots 1/2 (1 held)`.
Without a list, or off Linux, no slots are shown. The boss's `up 2h14m` counts
from when the boss's agent process started (A47), so it survives a daemon
restart and a `/clear`, while a new session or a handoff to another pane
starts it again. Where that start cannot be read, as off Linux and darwin, no
runtime is shown.

## The team's line in herdr's tab bar

`herdr-agentisan status line` prints herdr-status.sh's line for a team,
`PRs #878⏳ #886✓ · slots 2/2 · issues 14`, from the daemon's snapshot, so it
never calls gh (ADR-001 A41). A held slot reads as in the dashboard's header,
`slots 1/2 (1 held)` (A40). Run as herdr's `tab_bar_right` command, it
follows the focused workspace: the line is that workspace's team's, and
outside every team the entry hides. `--project <name>` shows one team
everywhere. What the daemon does not report is left out, and the line fits
the 80 characters herdr shows, the PRs giving way to a count first. A daemon
that is down, starting or slower than 1 s is one short line, such as
`agentisan: daemon down`, never a blank entry.

Put the `[ui]` keys of
[`docs/config/herdr-tab-bar.example.toml`](docs/config/herdr-tab-bar.example.toml)
in herdr's config, in place of herdr-status.sh's entry, and reload herdr's
config. herdr gives a tab-bar command no `HERDR_PLUGIN_STATE_DIR`, so the
command names the plugin's state directory with `--state-dir`, which the
shell works out from `XDG_STATE_HOME` or `HOME`, as herdr does. Retire
herdr-status.sh by hand after a live check; for the agentisan-skills team,
only once each project counts its own test slots.

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

`.devcontainer/` builds the local workflow into one image: Go 1.26.10, Task, uv
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
