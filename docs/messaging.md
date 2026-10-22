# Messaging

`herdr-agentisan send` is how the boss and its workers message each other
(ADR-001 D8, A2, A30). The daemon queues each message in its `state.db` and
types it into the recipient's agent once. Unlike `herdr agent prompt`, a
message survives a handoff: a worker's messages wait while its handoff is
pending, then go to the session that replaces it.

## Sending

```bash
herdr-agentisan send boss "PR #72 open, CI green"
herdr-agentisan send ha-coder-2 - <<'BODY'
Two lines
of body, from stdin.
BODY
```

| Recipient | Who it is |
| --- | --- |
| `boss` | Your own team's boss: `<short>-boss` for a tabs team, `boss` for the ◆ spaces |
| `<short>-boss` | That team's boss |
| A logical id, such as `ha-coder-2` | A worker that `worker spawn` started |

The command prints one JSON object, such as
`{"ok":true,"data":{"id":"<ulid>","state":"queued","to":{"kind":"boss","id":"ha-boss"},"from":{"kind":"worker","id":"ha-coder-1"}}}`.
`to` and `from` say whom the daemon took the message to be to and from. A
refusal exits 1:

| Code | Why |
| --- | --- |
| `unknown_recipient` | No live worker has the id and no team's boss has the name, or `boss` came from a pane in no team |
| `unknown_sender` | The command runs in no herdr pane it can confirm, and `HERDR_AGENTISAN_WORKER` names no live worker |
| `too_large` | The body is over 8 KiB |
| `bad_request` | No body, an empty one, or one with a NUL. Quote a body with spaces, put one starting with `-` after `--`, or pass `-` |
| `not_ready` | The daemon has just started; the command asks again for up to 10 s |
| `daemon_unavailable` | No daemon answers under `--state-dir`, `HERDR_PLUGIN_STATE_DIR`, or else herdr's state directory for the plugin, from `XDG_STATE_HOME` or `HOME` (A51). Fall back to `herdr agent prompt`, which is not durable |

## Who the sender is

The command names the pane it runs in, once its own process lineage
confirms it, and the daemon reads who that pane is from herdr and
`state.db`:
1. the worker a live row binds to the pane's terminal;
2. else that team's boss, for a pane in its `<short>-boss` tab or ◆ boss;
3. else the pane itself, by its terminal.

`HERDR_AGENTISAN_WORKER` counts only when the command's own pane can't be
confirmed, and only for a worker with a live row.

There is no authentication, as with `herdr agent prompt`. The daemon takes
the pane the command names, so any process of yours that reaches the
daemon's socket can claim any pane, and `HERDR_AGENTISAN_WORKER` is a claim
too.

## What arrives

- `[msg <ulid> from <sender>] <body>`: reply with
  `herdr-agentisan send <sender> …`.
- A message the daemon sends starts with `[agentisan` and takes no reply.
  `[agentisan] notices:`, then a line each, is what your team's workers and
  GitHub did (ADR-001 A44): a boss that is away gets one such notice, which
  grows until it is typed, and past 8 KiB its first line counts the oldest
  lines cut, `[agentisan] notices (3 earlier dropped):`.
  `[agentisan] message <ulid> to <recipient> may not have been delivered
  (<reason>); resend if needed: <body>` means its prompt timed out, failed
  after the text may have been typed, or the daemon stopped mid-prompt. Such
  a message is never retried: resend it if it still matters.
- A retired worker's queue goes to its boss, as
  `[msg <ulid> from <sender>, originally to <worker>] <body>`.
- The body is typed with CR and CRLF as LF, and with every other control
  character but LF and TAB left out.

## Pane senders

A pane that holds no worker and no boss sends as its terminal: its messages
arrive `from` the terminal's id, which `send` does not take as a recipient.
The daemon types its notices into that pane while an agent runs there. They fail, with
no further notice, when the pane has closed, or after an hour when no agent
runs in it. A herdr live handoff or restart gives every pane a new terminal,
so a pane sender's pending notices fail then.

## Limits

- A worker `worker spawn` did not start has no logical id the daemon knows,
  so `send <worker>` is refused. Use `herdr agent prompt` for it.
- A Codex worker in its default sandbox cannot reach a Unix socket. It
  receives messages but cannot send them, and the boss reads its turns
  from the daemon's notices.
- A delivered or failed message is kept 14 days.
