# Boss shift model check

NERD-5262 adds a model gate to the interim team script. `shift.sh open`
starts the candidate with its own `--session-id`. `shift.sh brief` pins the
Claude that runs with that id, once. `shift.sh go` and `shift.sh takeover`
ask the plugin whether that same process reports the requested model. Nothing
in the script waits for a report: when a check is refused, fix the cause and
run the command again.

```sh
herdr-agentisan boss shift check-model --pane "$pane" --terminal "$terminal" \
  --name "$candidate" --model fable --launched-at "$launch_ms" \
  --session-id "$uuid" [--pid "$claude_pid" --start "$claude_raw_start"]
```

Without `--pid` and `--start` the pane's one foreground Claude must carry
`--session-id <uuid>` (or `--session-id=<uuid>`) in its arguments; the reply is
`identified`, with that process's `pid` and `start` (the OS's raw start unit).
With them the call proves that same process is alive in the recorded terminal
and its pane reports the model in a report newer than the launch and the
process start and under 180 seconds old. The only alias is `fable`, which
accepts versioned Fable IDs including `[1m]`; other values must match exactly.
A refusal exits nonzero with a JSON verdict and a hint on stderr.

## Install between shifts

Finish or abort any open shift first. Verify the exact preimage SHA-256, then
dry run and apply the patch, from the matching plugin checkout.

```sh
script=~/.local/share/ha-team/boss-shift/shift.sh
printf '%s  %s\n' a9d92216ec0324c87372ed9fb38579568a57dcf72460d4e9366098abc3d38402 "$script" |
  sha256sum --check &&
  patch --batch --dry-run --fuzz=0 "$script" < docs/boss-shift-v1.patch &&
  patch --batch --fuzz=0 "$script" < docs/boss-shift-v1.patch
```

## What each command does

- `open` records the shift first (both terminals, the launch time, a fresh
  session id), then splits, then starts Claude with that id. It still peeks at
  the model the candidate reports, as advice only.
- `brief` names the Claude and saves its pid and start, then sends the first
  instructions. It never repins. If Claude is still starting it sends nothing:
  run it again.
- `go` and `takeover` check that process and its model before they act.
  `takeover` closes the old pane only while it still holds its recorded terminal.
- `status` shows the record and the plugin's verdict and writes nothing.
- `abort` closes the candidate's pane only while it still holds the recorded
  terminal; otherwise it says so and still ends the shift.

## By hand

- **A lost split reply.** The record exists before the split, so a failed or
  lost split leaves a record with no pane. `status` says so and `abort`
  closes nothing. Run `herdr pane list`, close any new pane yourself, then
  `abort`. A kill in the instant between creating the pane and saving its id
  needs the same cleanup.
- **An identity refusal** (`candidate_changed`). The pane no longer runs the
  Claude that was launched: abort and open again. If `abort` says it did not
  close the pane, that id belongs to something else now; leave it.
- **A missing or stale report.** `go` refuses with the reason. Wait for the
  next statusline report, fix the hook, or abort.
- **A brief while Claude still starts.** It refuses; run it again.

If GO was recorded but its cue failed, run `shift.sh go` again: it rechecks,
keeps the original GO and records a delivered cue in `GO-CUED`. A failed prompt
reply can be ambiguous about delivery. `SHIFT_MODEL_SEEN` no longer does anything.

## Known limits

The owner accepted one window: if someone switches the candidate with `/model`
after its last statusline report and before `takeover`, the check can still see
the old model and close the outgoing boss's pane. It lasts up to one refresh
(60 s on this team), or 180 s if reports stall. Verification, close and rename
are separate herdr calls, and a pane can change between the terminal check and
the close. Keep nested agents out of the candidate pane: the token belongs to
the pane. Effort is a launch argument.
