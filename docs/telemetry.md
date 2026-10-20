# Workflow events

The daemon records what happens to the team as workflow events (ADR-001 D10,
A21). They go over OTLP/HTTP as log records, each with an event name, so
Elastic stores them in its logs data streams, where the debugger agent
(item 12) queries them. Log records are not sampled, so unlike traces no event
is dropped by `HERDR_AGENTISAN_TRACE_SAMPLE_RATIO`.

This page is the schema's contract. A saved query on the Elastic side matches
these names, so renaming an event or a key is a contract change: update this
page, `internal/telemetry/events.go` and its test together.

## Where they go

| Setting | Where | Notes |
| --- | --- | --- |
| `[telemetry] endpoint` | the shared `herdr-agentisan.toml` in `HERDR_PLUGIN_CONFIG_DIR`, or `--set` | `host:port`, such as `localhost:4318`. Not allowed in a project table or a repo file: there is one daemon per herdr server |
| `HERDR_AGENTISAN_OTLP_ENDPOINT` | the daemon's environment | Overrides the key when set, and must be `host:port` too. Traces use it too |

With neither set, nothing is built and nothing is exported, which is the
default. The file is the way to turn events on for a running herdr: the daemon
inherits herdr's environment, through the `[[startup]]` hook or the
`daemon-restart` action, so a variable exported in a shell later never reaches
it, and restarting herdr is not an option (I7).

The daemon reads the endpoint once, when it starts; run the `daemon-restart`
action after changing it. `daemon.log` says `workflow events export over OTLP`
with the endpoint when events are on.

An endpoint that is not `host:port` is refused, and the error never repeats
it, since a `user:password@` in it would otherwise land in `daemon.log`. The
exporter takes nothing from `OTEL_EXPORTER_OTLP_*`: herdr's environment may
carry those for another program, and they must not redirect the events or
attach their headers.

The connection is plaintext with no credentials: the collector is expected on
localhost and holds Elastic's own key. The slot for a collector across the
network, a `token = { env = "…" }` or `{ op = "op://…" }` reference sent as
`Authorization` over TLS, arrives when one is needed; until then any key but
`endpoint` in `[telemetry]` is an unknown key.

Telemetry never stops the daemon. A file that does not resolve is logged and
the daemon runs without events; a collector that is down or refuses an export
is a `telemetry export` warning in `daemon.log`. On exit the daemon flushes
what is pending for at most 2 s, after it has released its lock, so a dead
collector never holds up the next daemon.

## Events

Every event carries the resource traces carry: `service.name`
(`herdr-agentisan`), `service.version`, `service.commit` and
`deployment.environment`. Severity is `INFO`.

### `agentisan.worker.status_changed`

A pane's agent status changed between two polls of the daemon (every 3 s), or
the pane appeared in or left the team model. The body reads
`<worker id> <from> → <to>`.

| Attribute | Type | Value |
| --- | --- | --- |
| `agentisan.worker.id` | string | The logical worker id `state.db` binds to the pane, or the pane id while no row binds it |
| `agentisan.worker.bound` | bool | Whether a `workers` row binds the pane |
| `agentisan.project` | string | The project the pane's space belongs to (`[spaces] project`); `""` is the unnamed project |
| `agentisan.group` | string | The group, such as `coders`; `""` for the boss |
| `agentisan.role` | string | `boss` or `worker` |
| `agentisan.agent` | string | The agent herdr detected, such as `claude` or `codex`; `""` for a shell |
| `herdr.pane.id` | string | The pane, such as `wN:p1` |
| `agentisan.status.from` | string | `working`, `idle`, `done`, `blocked`, `unknown`, or `absent` for a pane that appeared |
| `agentisan.status.to` | string | The same set, `absent` for a pane that left |

How to read them:

- **The first poll after the daemon starts is a baseline** and emits nothing,
  so a restart is not every pane appearing. Changes while the daemon was down
  are not recorded.
- **A poll that fails to list the panes or workspaces** emits nothing and
  leaves the baseline as it was; the next good poll reports the change against
  it.
- **Changes are found by pane, not by logical id.** A rotation (item 5b) reads
  as the old pane leaving and the new one appearing, both under the same
  `agentisan.worker.id`. A poll that could not read `state.db` reports any
  change under the pane id, with `agentisan.worker.bound` false.
- **A pane that moves to another group, keeping its status,** gives no event.
  A pane moved out of every ◆ space leaves (`to` is `absent`).

A query for one worker's last hour, in ES|QL:

```
FROM logs-*
| WHERE event_name == "agentisan.worker.status_changed"
    AND attributes.agentisan.worker.id == "coders-1"
    AND @timestamp > NOW() - 1 hour
| SORT @timestamp
| KEEP @timestamp, attributes.herdr.pane.id, attributes.agentisan.status.from, attributes.agentisan.status.to
```

Field names on the Elastic side depend on its OTLP mapping; check one event in
Discover before saving a query.

## Coming

The same helper carries these when their items land: stage changes (fast
follow on item 3), context threshold crossings (item 5b), and handoff steps
and rotations (items 5b and 6). Each adds a section here first.
