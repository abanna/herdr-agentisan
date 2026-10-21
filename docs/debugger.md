# The debugger

The debugger is a Claude agent that answers questions about the team's
workflow from evidence (ADR-001 item 12, A32). It ships with the plugin:

- its role is [`roles/debugger.md`](../roles/debugger.md);
- `herdr-agentisan events` reads the workflow event trail
  ([`telemetry.md`](telemetry.md)) back out of Elasticsearch;
- `internal/debugger` builds its read-only launch, and `worker spawn --group
  debugger` will start it (item 4).

It reads the trail, the daemon's log, the project's repo and Linear, and it
can change nothing. The `events` commands work for anyone with the env file
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

`worker spawn --group debugger` launches it in the project's repo (part 2,
after item 4). Until then, `herdr-agentisan debugger args` prints the same
launch for a start by hand:

```
herdr-agentisan debugger args --repo ~/src/project --server-dir <the daemon's srv-… dir> [--config-dir <dir>] [--json]
```

That prints one shell line: `HERDR_PLUGIN_CONFIG_DIR=… claude …`. Run it
from the project's repo. The parts of the launch:

- **The role,** appended from the plugin checkout with
  `--append-system-prompt-file <root>/roles/debugger.md`. `--root` defaults to
  `$HERDR_PLUGIN_ROOT`, then to the checkout the binary was built in.
- **The session's facts,** in a second prompt: the plugin binary's path,
  the repo, and `daemon.log` when `--server-dir` names the daemon's directory
  (`$HERDR_PLUGIN_STATE_DIR/srv-<id>`).
- **`--permission-mode dontAsk`,** which denies every tool without an allow
  rule, with no prompt. The allow rules are:
  - reading the repo and the daemon's directory;
  - running `<bin> events`;
  - Linear's read tools (an issue, issues, an issue's comments).

  The `events` rule matches no command behind an environment assignment,
  such as `HERDR_PLUGIN_CONFIG_DIR=… <bin> events …`, or behind `env`: an
  allow rule matches past an assignment only of the variables Claude Code
  counts as safe, so the debugger cannot point `events` at another env file.
- **Denied outright:** reading the repo's `.env*` files, Edit, Write,
  NotebookEdit, WebFetch and WebSearch.
- **The environment:** `HERDR_PLUGIN_CONFIG_DIR`, so `events` finds the env
  file. There is no URL and no key in it.

It is still Claude in the project's repo, so D12 holds and the repo's
Agentisan bundle loads. With no allow rule its skills cannot run, which is
intended.
