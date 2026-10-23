# The debugger

The debugger is a Claude agent that answers questions about the team's
workflow from evidence (ADR-001 item 12, A32). It ships with the plugin:

- its role is [`roles/debugger.md`](../roles/debugger.md);
- `herdr-agentisan events` reads the workflow event trail
  ([`telemetry.md`](telemetry.md)) back out of Elasticsearch;
- `internal/debugger` builds its read-only launch, and `worker spawn --group
  debugger` starts it as one of the team's workers.

It reads the trail, the daemon's log, the project's repo and Linear. Its
tools write nothing, and its reads are fenced to the repo and the daemon's
directory. What still reaches past that, the Linear tools' free-text
arguments, the repo's hooks, mods and managed policy, is listed under
"Launching it". The `events` commands work for anyone with the env file
below: a person, the boss, or the debugger.

## `herdr-agentisan events`

| Command | What it answers |
| --- | --- |
| `events worker <id> [--since 1h] [--json]` | Every status change of one worker or pane in the window, oldest first: at most the newest 1,000, and it says when it cut older ones. `<id>` is a logical id or a pane id: the id matches either, because a worker's events carry its pane id until a worker row binds it. `--since` takes whole minutes, from 1m to 168h |
| `events query '<ES\|QL>' [--json]` | Any ES\|QL read of the trail. It must start `FROM logs-agentisan.otel-*`, followed by a pipe or nothing: no second source, and the words `LOOKUP` and `ENRICH` nowhere. The read key's privileges bound everything else |

`events worker w14:p8` prints one line per change, such as
`2026-10-20T11:29:10.415Z  w14:p8  done → working  coder  unbound`: the time,
the worker (and its pane when the two differ), from → to, the group, and
whether a worker is bound. An empty answer says so. A worker that changed
nothing, a wrong id and a daemon that was not running all read the same, so
check the daemon's log before concluding anything.

## Where the URL and the key come from

They read `ELASTIC_URL` and `ELASTIC_API_KEY` at each query, and from one
place only: the env file named by `[debugger] env_file` in the shared
`herdr-agentisan.toml`. They find that file through `HERDR_PLUGIN_CONFIG_DIR`.

```toml
[debugger]
env_file = "~/.config/nerdsrun/local.env"
```

- **What the file is.** It is an absolute path, or one under `~/`, on this
  machine, and never in a repo. A dotenv file, or a file 1Password mounts,
  both work.
- **What is taken from it.** Only those two names. Every other line,
  another secret included, is passed over.
- **The key never enters an agent's environment.** It is read when a query
  runs, sent only to that file's URL, never after a redirect, and printed
  nowhere.
- **No flag can redirect it.** No flag of `events` names a URL, a variable or
  a file, so an agent allowed to run them cannot send the key anywhere else.
  `TestEventsFlagsNameNoDestination` fails on any new flag.
- **A file that does not answer fails the query.** A mounted file is
  answered only while 1Password is unlocked. After 10 s the query fails,
  saying so.
- **The URL** is `http(s)://host[:port]` with nothing more, and the env file
  carries no user in it.
- **The key** needs only `read` on `logs-agentisan.otel-*`. A key with
  `read` on `logs-*` also works.

From a shell outside herdr, export the config dir first:
`HERDR_PLUGIN_CONFIG_DIR=<herdr's plugin config dir> herdr-agentisan events
worker <id>`.

## Launching it

`herdr-agentisan worker spawn --group debugger` starts it as one of the
team's workers (ADR-001 A32, A23). It gets a `<short>-debugger-<n>` tab in
the team's workspace, in the project's repo, and a `workers` row, so the
dashboard shows it in its group and `/` finds it by name. The group needs a
slot, and the debugger needs its env file:

```toml
[groups.debugger]        # or [projects.<p>.groups.debugger]
workers = 0              # team up starts none
slots = 1
# agent = { model = "sonnet" }   # a model only: the launch is the plugin's

[debugger]
env_file = "~/.config/herdr-agentisan/elastic.env"
```

Spawn builds the launch below from the daemon's plugin checkout and binary,
the repo, the daemon's directory and the env file, then adds the
workforce's `--model` and `--name`. Its group sets no command, and the
top-level `[agent] command` never applies to it: a flag there, such as a
permission mode, would lift its fence. A launch spawn cannot build is
refused whole, with `config_invalid`, before any tab opens: no `env_file`,
no role (a regular file) under the checkout, the env file in the repo or the
daemon's directory, an `.mcp.json` it cannot read, or a path a tool rule
cannot carry. `team up` fills a group whose `workers` is above 0 with the
same launch, and one it cannot build refuses the whole team up only when a
debugger is to open. Its model is `--model`, else its group's, else `[agent]`'s
when `[agent]`'s command is claude; while `[routing] enabled` is on, routing's
`alex_debugger` role chooses its model and effort (ADR-001 A50). A debugger
reports its item and stage through A42's turn hooks, which spawn puts into
its launch's one `--settings` object beside the read fence, but it reports
no `ctx` and writes no handoff, so retire it with a plain
`worker retire <id>`:
`--after-handoff` is refused for one bound to a pane (an unbound one is
retired with nothing closed, as any is).

`herdr-agentisan debugger args` prints the same launch for a start by hand,
outside the team:

```
herdr-agentisan debugger args --repo ~/src/project --server-dir <the daemon's srv-… dir> [--config-dir <dir>] [--json]
```

That prints one shell line, `cd <repo> && HERDR_PLUGIN_CONFIG_DIR=… claude …`,
which you can run from anywhere: it starts claude in the repo, because the
directory claude starts in is one its read fence admits. `--json` prints the
same as `dir`, `env` and `argv`. The parts of the launch:

- **The role,** appended from the plugin checkout with
  `--append-system-prompt-file <root>/roles/debugger.md`. `--root` defaults to
  `$HERDR_PLUGIN_ROOT`, then to the checkout around the binary
  (`<root>/bin/herdr-agentisan`). An installed copy, from `task install` or
  `go install`, sits in no checkout, so run the checkout's binary or pass
  `--root`; the error says where the root it tried came from.
- **The session's facts,** in a second prompt: the plugin binary's path,
  the repo, and `daemon.log` when `--server-dir` names the daemon's directory
  (`$HERDR_PLUGIN_STATE_DIR/srv-<id>`).
- **`--permission-mode dontAsk`,** which denies, with no prompt, every call
  that would ask. Claude Code's built-in read-only commands, such as `ls`,
  `cat`, `head`, `tail`, `grep`, `find` and read-only `git`, need no rule and
  run inside the read fence below. The allow rules are:
  - reading the repo and the daemon's directory;
  - running `<bin> events`;
  - Linear's read tools (an issue, issues, an issue's comments).

  The `events` rule matches no command behind an environment assignment,
  such as `HERDR_PLUGIN_CONFIG_DIR=… <bin> events …`, or behind `env`: an
  allow rule matches past an assignment only of the variables Claude Code
  counts as safe, so the debugger cannot point `events` at another env file.
  The live check on #76 (2026-10-22) saw `dontAsk` deny
  `<bin> events query "$(cat …)"`, whose nested `cat` read a file outside
  the read fence, while `<bin> events query 'FROM … | LIMIT 1'` ran. It did
  not try a substitution whose nested command stays inside the fence; such a
  command reads nothing the fence does not already admit, and the env file
  lies outside it. Claude Code's reference documents only that deny and ask
  rules see the nested command.
- **Its built-in tools, by allowlist:** `--tools Bash,Read,ToolSearch`, so
  the session has those three and none of Claude Code's other built-in
  tools: no Edit, Write, Agent, Skill, LSP or Cron tool, and no tool a later
  Claude Code adds. `EndConversation`, which only ends the session, cannot
  be removed. `ToolSearch` stays so that Linear's tools load when asked for
  rather than all at start. `--tools` does not reach MCP tools. Whether
  `ToolSearch` can load a built-in tool `--tools` leaves out is not
  documented; NERD-5370's live check settles it, and the deny lists below
  stay as the second layer.
- **Denied outright:** reading the repo's `.env*` files and the env file
  (by its name, and its target when it is a link), Edit, Write, NotebookEdit,
  WebFetch and WebSearch. A redirect such as `> file` is checked as an Edit,
  so it is denied too. The launch refuses a repo or daemon directory that
  holds the env file, such as your home directory, and needs `env_file` set.
- **Denied by name as well, Claude Code's own tools that act without asking:**
  `Agent`, `Workflow`, `Monitor`, `TaskStop`, `EnterPlanMode`, the `Cron` tools,
  `ScheduleWakeup`, `RemoteTrigger`, `SendMessage`, `ListAgents`,
  `PushNotification`, `SendUserFile`, `SendFeedback`, the `Artifact` tools,
  `DesignSync`, `EnterWorktree`, `ExitWorktree`, and the MCP resource tools
  `ListMcpResourcesTool`, `ReadMcpResourceTool` and `ReadMcpResourceDirTool`.
  Most need no permission, so `dontAsk` never stops them: a live check found
  `CronList` running before they were denied. A deny rule naming a tool
  removes it from the session. `--tools` already leaves them out, and a tool
  a later Claude Code adds with them: this list is the second layer.
- **Its MCP servers, by allowlist:**
  `"allowedMcpServers":[{"serverUrl":"https://mcp.linear.app/*"}]` in
  `--settings`, so Linear's claude.ai connector is the only server that
  loads. An allowlist covers claude.ai's connectors too, matched by URL,
  since a connector's name is claude.ai's display name. Your other
  claude.ai connectors, a plugin's servers and the project's stay out;
  built-in servers, such as Claude in Chrome and the IDE server, skip an
  allowlist. An allowlist in the repo's `.claude/settings.json` or in
  managed policy would add to it. Linear's tools that write are denied by
  verb, which removes them: `mcp__claude_ai_Linear__save_*`, and the same
  for `delete`, `create`, `update`, `merge`, `submit`, `share`, `unshare`,
  `mark`, `resolve`, `restore`, `retire` and `prepare`. Its reads begin
  `get_`, `list_`, `search_` or `extract_`. A verb Linear adds later is not
  covered; `dontAsk` still refuses its tools, since no rule allows them.
- **The project's MCP servers, rejected by name:** the launch reads the
  `.mcp.json` in the repo and in each directory above it, as Claude Code
  does, and names every server in them in `--settings`'
  `disabledMcpjsonServers`. So none loads, and the session starts without
  asking you to approve them. A `.mcp.json` it cannot read or parse refuses
  the launch, and a server added after the launch was printed would ask.
  It does not pass `--strict-mcp-config`: that keeps claude.ai's
  connectors out too, and the debugger reads Linear through one. Your own
  MCP servers stay out as well, since Claude Code loads them only with your
  user and local settings. Your other claude.ai connectors and a plugin's
  servers are kept out by the allowlist above.
- **The read fence:** `{"permissions":{"blockReadsOutsideWorkingDirectories":true}}`
  in `--settings`, and `--add-dir <the daemon's directory>`. Without it, Claude Code's built-in
  read-only commands, such as `cat` and `grep`, would read any file you can,
  with no allow rule. With it, the file tools refuse a read outside the
  working directories (the repo and the daemon's directory), except the
  files Claude Code itself needs under `~/.claude/`, such as its skills and
  `CLAUDE.md`, and a file-reading command outside them asks, which `dontAsk`
  denies. It needs Claude Code 2.1.257 or later, and a live check on 2.1.306
  saw a `cat` outside it refused. There is no Grep or Glob tool: `--tools`
  leaves both out, so the debugger searches with `grep` and `find` in the shell, as read-only
  commands inside the fence. Inside the fence, a command that reads without
  naming a file, such as `grep -r` run from a directory, is not checked
  against the deny rules: `grep -r` in the repo reads its `.env*` files. So
  the env file must stay outside both directories, which the launch
  enforces.
- **`--setting-sources project`:** of the settings files, only the repo's
  `.claude/settings.json` loads, as it stands in the working tree, whatever
  git tracks, so the "don't ask again" rules that pile up in
  `.claude/settings.local.json` and your own `~/.claude/settings.json` never
  widen it. Your statusline and hooks do not run in it either, so its pane
  shows no `ctx`.
- **The environment:** `HERDR_PLUGIN_CONFIG_DIR`, so `events` finds the env
  file. There is no URL and no key in it.
  The rest of your shell's environment is kept, so a secret exported in
  the shell you launch from is in the debugger's process: launch from one
  that exports none. Only the allow list and `dontAsk` keep the debugger
  from reading it; a live check saw an `echo` of an exported variable
  refused, which the docs do not promise.

It is still Claude in the project's repo, so D12 holds and the repo's
Agentisan bundle loads, but not its MCP servers. Its skills cannot run, since
`--tools` leaves out the Skill tool, which is intended.

What still reaches past the allow list and the fence:

- **The Linear tools' free-text arguments.** These are `get_issue`'s id,
  `list_issues`' query and `list_comments`' issue id. Anything the debugger
  has read (the trail, the repo, `daemon.log`) can leave the machine in one,
  so a prompt injection in any of those can send to Linear what the fence
  admits. The role tells it to pass only ids and your own words.
- **The repo's hooks.** Its `.claude/settings.json` hooks run in every
  debugger session. In this repo git tracks `{}`, while the working tree
  holds the Agentisan bundle's hooks; their SessionStart hook rewrites
  `.claude/hooks/.handoff-dirty` and `.handoff-attempts` at each start.
- **Mods.** A mod you installed can always approve a call that only
  `dontAsk` would deny. Without managed settings, and without a Team or
  Enterprise login as the session's credential, it can also approve a call
  a deny rule refuses or the fence would ask for: it can lift every rule
  here, the env file's deny included. The launch does not choose which mods
  load.
- **Managed policy and that file's allow rules.** An allow rule in either
  widens the debugger too, so check the file, not only what git tracks.
