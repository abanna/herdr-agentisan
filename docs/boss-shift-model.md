# Boss shift model check

NERD-5262 adds a model gate to the interim team script. `open` starts the
candidate with its own `--session-id`. `brief` pins the Claude that carries it,
once, and proves its model before sending anything; `go` and `takeover` prove
both again. Nothing waits for a report: when a check is refused, fix the cause
and run the command again.

```sh
herdr-agentisan boss shift check-model --pane "$pane" --terminal "$terminal" \
  --name "$candidate" --model fable --launched-at "$launch_ms" \
  --session-id "$uuid" [--pid "$claude_pid" --start "$claude_raw_start"]
```

Without `--pid` and `--start` the pane's one foreground Claude must carry
`--session-id <uuid>` (or `--session-id=<uuid>`); the reply is `identified`,
with its `pid` and `start` (the OS's raw unit). With them the call proves that
process is alive in the recorded terminal and the pane's model report is newer
than the launch and the process start, under 180 s old, and from this session:
the `model` token is the pane's, so the report must carry `model_session`
equal to the launch id, which `report statusline` writes from the statusline's
`session_id`. The only alias is `fable`, which accepts versioned Fable IDs
including `[1m]`; other values match exactly. A refusal exits nonzero with a
JSON verdict and a hint.

## Install between shifts

Finish or abort any open shift; install a build whose `report statusline`
writes `model_session`. Check the preimage SHA-256, then dry run and apply.

```sh
script=~/.local/share/ha-team/boss-shift/shift.sh
printf '%s  %s\n' a9d92216ec0324c87372ed9fb38579568a57dcf72460d4e9366098abc3d38402 "$script" |
  sha256sum --check &&
  patch --batch --dry-run --fuzz=0 "$script" < docs/boss-shift-v1.patch &&
  patch --batch --fuzz=0 "$script" < docs/boss-shift-v1.patch
```

## What each command does

- `open` makes a shift folder of its own, `<root>/YYYYMMDD-HHMMSS-XXXXXX`, and
  claims it by making `current` a link to it, so an aborted open can never
  write into its replacement's folder. It records the old terminal, launch time
  and session id, then splits, saves the candidate's pane and terminal, and
  starts Claude with that id. It reads no model. If the shift ended during the
  split or the start (`current` gone, or `ABORTED` in the folder) it closes the
  new pane and stops.
- `brief` saves the Claude's pid and start once, then proves it and its model
  on every run, and only then sends the instructions.
- `go` and `takeover` prove the same again. `takeover` looks up the old pane's
  terminal first, proves the candidate last, and closes the pane only while it
  holds the terminal recorded at `open`; `abort` does the same for the
  candidate's, and only for a shift that has not ended.
- `status` shows the record and the verdict; it writes nothing.

## By hand

- **A lost split reply.** The record exists before the split, so a failed or
  lost split leaves one with no pane. `status` says so and `abort` closes
  nothing. Run `herdr pane list`, close any new pane yourself, then `abort`. A
  kill between the split and saving its pane needs the same cleanup.
- **An identity refusal** (`candidate_changed`). The pane no longer runs the
  launched Claude: abort and open again. If `abort` says it did not close the
  pane, look with `herdr pane list` before closing anything.
- **A missing, stale or foreign report.** Wait for the next statusline report,
  fix the hook, or abort. A refused `brief` sent nothing: run it again.

If `go` cannot write GO it stops before any cue. If GO was recorded but its cue
failed, run `go` again: it rechecks, keeps the GO and records a delivered cue
in `GO-CUED` (a failed prompt reply can be ambiguous). `SHIFT_MODEL_SEEN` no longer does anything.

## Known limits

The owner accepted one window: a `/model` switch after the candidate's last
statusline report and before `takeover` is not seen, so the check can still
close the outgoing boss's pane. It lasts up to one refresh (60 s here), or
180 s if reports stall. Verification, close and rename are separate herdr
calls, and a pane can change between the terminal check and the close. The
session proof assumes the statusline's `session_id` equals `--session-id`
(herdr's own `agent_session` does); if it differs, or a `/clear` gives the
candidate a new one (unverified), every check refuses `wrong_session`: replay
once at activation.
