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
| `[telemetry] endpoint` | the shared `herdr-agentisan.toml` in `HERDR_PLUGIN_CONFIG_DIR`, or `--set` | `host:port`, such as `127.0.0.1:4328`, the collector below. Not allowed in a project table or a repo file: there is one daemon per herdr server |
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
localhost and holds Elastic's own key, as the one below does. The slot for a
collector across the network, a `token = { env = "…" }` or `{ op = "op://…" }`
reference sent as `Authorization` over TLS, arrives when one is needed; until
then any key but `endpoint` in `[telemetry]` is an unknown key.

Telemetry never stops the daemon. A file that does not resolve is logged and
the daemon runs without events; a collector that is down or refuses an export
is a `telemetry export` warning in `daemon.log`. On exit the daemon flushes
what is pending for at most 2 s, after it has released its lock, so a dead
collector never holds up the next daemon.

## A collector into Elasticsearch

`docs/config/otelcol-agentisan.yaml` is a collector for these events:
otelcol-contrib with an OTLP/HTTP receiver on `127.0.0.1:4328` and one logs
pipeline into Elasticsearch, in the `otel` mapping. Each record lands in the
data stream `logs-agentisan.otel-default`: the collector sets
`data_stream.dataset` to `agentisan` and `data_stream.namespace` to `default`
on every resource, and the exporter appends `.otel` to the dataset. It takes
logs only, so traces sent to it through `HERDR_AGENTISAN_OTLP_ENDPOINT` are
refused.

It reads two variables, and nothing secret is in the file:

| Variable | Value |
| --- | --- |
| `HERDR_AGENTISAN_ES_URL` | Elasticsearch's URL, such as `https://elastic.example:9200`, with no `user:password@` in it: the collector prints a URL it cannot use in its error |
| `HERDR_AGENTISAN_ES_WRITE_KEY` | The `encoded` form of an API key that can only write this stream |

Create the key with index privileges `create_doc` and `auto_configure` on
`logs-agentisan.otel-*` and no cluster privileges, so it can add events to
the one stream, and create it on the first write, but read nothing:

```
POST /_security/api_key
{
  "name": "herdr-agentisan-collector",
  "role_descriptors": {
    "agentisan-events-writer": {
      "indices": [
        { "names": ["logs-agentisan.otel-*"], "privileges": ["create_doc", "auto_configure"] }
      ]
    }
  }
}
```

With both variables exported, `task otelcol:up` in the plugin's checkout runs
the collector in Docker as `herdr-agentisan-otelcol`, after validating the
file in a throwaway container, and replaces one already running; `task
otelcol:down` removes it. The container reads the file from that checkout,
so run it from one you keep: once the file is gone, the container cannot
start again. Without Task, the same container is:

```
docker run -d --name herdr-agentisan-otelcol --network host --restart unless-stopped \
  -e HERDR_AGENTISAN_ES_URL -e HERDR_AGENTISAN_ES_WRITE_KEY \
  --mount "type=bind,src=$PWD/docs/config/otelcol-agentisan.yaml,dst=/etc/otelcol/agentisan.yaml,readonly" \
  otel/opentelemetry-collector-contrib:0.159.0 --config=/etc/otelcol/agentisan.yaml
```

and without Docker, `otelcol-contrib --config
docs/config/otelcol-agentisan.yaml`. Then set `[telemetry] endpoint =
"127.0.0.1:4328"` and run the `daemon-restart` action. An event reaches
Elasticsearch about 11 s after the change: the daemon batches for 1 s and the
exporter for 10 s.

Where a failure shows:

- **The collector is down:** a `telemetry export` warning in `daemon.log`.
- **Elasticsearch is down or refuses the key:** the collector's log, `docker
  logs herdr-agentisan-otelcol`. The exporter retries, then drops the batch.
  The daemon sees a collector that accepted its records and logs nothing,
  until the collector's queue fills and it refuses them too.

Mind three things:

- **The key is on disk while the container exists.** The variables reach it
  by name, never as a value on a command line, and never through
  `--env-file`, which would hand it every other variable in that file. But
  Docker keeps a container's environment in its config, where `docker
  inspect` shows it to anyone who can run Docker; `--restart unless-stopped`
  is what brings the collector back after a reboot with no one there to give
  it the key. `task otelcol:down` removes that copy.
- **The receiver is plaintext and unauthenticated**, on loopback only: any
  local process can write into the stream. The key's privileges keep every
  write inside it.
- **Use `https://` for an Elasticsearch across a network.** The key goes with
  every request.

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

A query for one worker's last hour, in ES|QL, against the stream the
collector above writes:

```
FROM logs-agentisan.otel-*
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
