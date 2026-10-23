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
| `caller_unverified` | The command runs under Codex, but not in its pane's own Codex, so its pane and `HERDR_AGENTISAN_WORKER` may be another session's: start Codex with `--no-daemon`, and run `send` outside its sandbox (A64). Nothing was queued, and the daemon was never asked |
| `too_large` | The body is over 8 KiB |
| `bad_request` | No body, an empty one, one with a NUL, or one that is not UTF-8. Quote a body with spaces, put one starting with `-` after `--`, or pass `-` |
| `config_invalid`, `bypasses_agentisan` | The daemon could not read the shared config, or it starts an agent that skips the Agentisan bundle. Nothing was queued: fix the config, then send again |
| `herdr_failed` | A herdr call failed, finding the sender or reading the layout. Nothing was queued: send again |
| `herdr_gone` | The daemon's herdr server is gone or was replaced. Nothing was queued: run the daemon-restart action, then send again |
| `store_failed` | The daemon could not read or write `state.db`. Nothing was queued: send again, and tell the boss if it goes on |
| `timeout` | The op ran past its 10 s budget while a call failed. Nothing was queued: send again |
| `unknown_op` | The daemon is a build without `send`. Nothing was queued: run the daemon-restart action, or fall back to `herdr agent prompt`, which is not durable |
| `unsupported_version` | The daemon is another build of the plugin. Nothing was queued: run the daemon-restart action, then send again |
| `daemon_no_answer` | The daemon took the request and gave no answer: it was killed, or ran past the command's wait (A52). The message may be queued, and a queued message is delivered once the daemon runs again, so don't resend it with `herdr agent prompt`. If the daemon isn't running, run the daemon-restart action: a message still queued is delivered then, and one the daemon was typing when it stopped is failed at the next start, and its sender told |
| `failed` | An answer no other code names, or one the command could not read. The message may be queued, as for `daemon_no_answer` |
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

Under Codex, the pane and `HERDR_AGENTISAN_WORKER` count only from the pane's
own Codex, `codex --no-daemon` as spawned workers run. On the shared
app-server both are the session's that started the daemon, so `send` is
refused `caller_unverified` there (A64).

Any pane in the boss's place sends as the boss: a shell split in the
`<short>-boss` tab, or a second tab of that name. The boss can also
`send boss` to itself.

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
  lines cut, `[agentisan] notices (3 earlier dropped):`. It keeps its place
  in your inbox as it grows, so a line in it can be newer than a message
  typed after it.
  `[agentisan] message <ulid> to <recipient> may not have been delivered
  (<reason>); resend if needed: <body>` means its prompt timed out, failed
  after the text may have been typed, or the daemon stopped mid-prompt. Such
  a message is never retried: resend it if it still matters.
- A retired worker's queue goes to its boss, as
  `[msg <ulid> from <sender>, originally to <worker>] <body>`, and a
  message the daemon sent it as `[agentisan] originally to <worker>: <body>`.
- The body is typed with CR and CRLF as LF, and with every other control
  character but LF and TAB left out.
- A text that ends on a word starting with `@` or `$` is typed with one
  space after it, so Codex opens no file or mention popup to take the
  Enter. Claude and Codex trim it.
- The header in front of a body, `[msg <ulid> from <sender>]` with or
  without `, originally to <worker>`, or `[agentisan] originally to
  <worker>:`, doesn't hide an `item:` line that opens the body: `report
  turn` reads it there, so a dispatch can send its item first (A42). A
  Codex steer (below) runs Codex's UserPromptSubmit hooks when the turn
  takes it, so `report turn` reads its `item:` then (Codex 0.163.0). One
  that opens a resend notice's quoted body is not: the notice's own words
  come first on that line.

## A recipient that is working

The daemon doesn't wait for the recipient's turn to end: herdr types a
message into a working agent at once (ADR-001 A30). `delivered` means the
agent's input took the text, not that the agent has acted on it.

- A Codex agent takes it into its running turn as a steer, and reads it
  at the turn's next step, once its current tool calls return, so the
  turn can change course before it ends. Codex may first list it under
  "Messages to be submitted after next tool call", then shows it in the
  transcript as your message. During a review or a compaction it holds
  it under "Messages to be submitted at end of turn" instead.
- In the live check (Codex 0.163.0) the two ended as one turn: the boss
  hears of one turn's end, and the stage comes from that turn's last
  message.
- A message meant for after the current task says so: "when you finish
  the current task, …".

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
- A Codex worker in a network sandbox (Codex's `workspace-write` or
  `read-only`, ADR-001 A59) cannot reach a Unix socket. It receives
  messages but cannot send them: `send` answers
  `daemon_unavailable`, saying the sandbox is why and to ask the boss
  (ADR-001 A25), and the boss reads its turns from the daemon's notices.
- A delivered or failed message is kept 14 days.
