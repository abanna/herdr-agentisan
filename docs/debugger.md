# The debugger's trail

The debugger is a Claude agent that answers questions about the team's
workflow from evidence (ADR-001 item 12). Its first tools ship here:
`herdr-agentisan events` reads the workflow event trail
([`telemetry.md`](telemetry.md)) back out of Elasticsearch. The debugger's
role and its read-only launch follow, and `worker spawn --group debugger`
will start it (item 4). The commands work for anyone with the env file below:
a person, the boss, or the debugger.

## `herdr-agentisan events`

| Command | What it answers |
| --- | --- |
| `events worker <id> [--since 1h] [--json]` | Every status change of one worker or pane in the window, oldest first. `<id>` is a logical id or a pane id: the id matches either, because a worker's events carry its pane id until a worker row binds it. `--since` takes whole minutes, from 1m to 168h |
| `events query '<ES\|QL>' [--json]` | Any ES\|QL read of the trail. It must start `FROM logs-agentisan.otel-*` |

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
  runs, sent only to that file's URL, and printed nowhere.
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
