# Boss shift model check

NERD-5262 adds a model gate to the interim team script. The gate reads
the candidate's Claude pane, ties the report to the recorded launch time
and current Claude process, and requires a report less than 180 seconds
old. The only alias is `fable`, which accepts versioned Fable IDs including
their `[1m]` variants. Other model values must match the reported ID
exactly. A refusal exits nonzero with a JSON verdict and a hint on stderr.

```sh
herdr-agentisan boss shift check-model --pane "$pane" --terminal "$terminal" \
  --name "$candidate" --model fable --launched-at "$launch_ms" \
  --pid "$claude_pid" --start "$claude_raw_start" --pinned-at "$pin_ms"
```

An initial call omits the three process flags or supplies all as zero.
Open pins the first Claude it observes with a nonzero `pid` and `start`.
Take `--pinned-at` in Unix milliseconds after the observation. Pass all
three values on subsequent checks. The process may start at most one
second before the recorded launch, allowing for clock rounding.
Verification requires a report strictly more than six seconds after
the pin, allowing an in-flight report from the previous process to
finish. Inspect `requested_model`, `observed_model` and `reason` to
diagnose a refusal.

## Install between shifts

Install a build with `boss shift check-model` and finish or abort any
open shift before changing the script. The adapter needs the split reply's
terminal ID and the candidate record it creates at open. Verify the exact
preimage SHA-256, then dry run and apply this patch to the team's existing
script. Run from the matching plugin checkout.

```sh
script=~/.local/share/ha-team/boss-shift/shift.sh
printf '%s  %s\n' a9d92216ec0324c87372ed9fb38579568a57dcf72460d4e9366098abc3d38402 "$script" |
  sha256sum --check &&
  patch --batch --dry-run --fuzz=0 "$script" < docs/boss-shift-v1.patch &&
  patch --batch --fuzz=0 "$script" < docs/boss-shift-v1.patch
```

Use a shell timeout of at least 300 s for `shift.sh open`. It usually
finishes in under about 75 s, can take about 160 s with a late start,
and ends within five minutes. Open needs a statusline refresh interval
of about 90 s or less in the candidate's settings.

If open reaches its deadline or the caller's shell kills it, run
`shift.sh status`. It shows the verdict and saved pin without writing
shift state. If the pin
was saved, continue the usual handoff. `shift.sh go` verifies the pin
and report again. If the pin is missing, abort and open again.
Ignore the old `SHIFT_MODEL_SEEN` instruction. If a post-GO check
fails, restore verified proof before retrying takeover.
If GO was recorded but cueing failed, rerun `shift.sh go` from the
outgoing pane to recheck the candidate and resend the cue. The script keeps
the original GO and records a successful cue in `GO-CUED`. A failed
prompt response can be ambiguous about whether the cue arrived.

Takeover automatically closes the old pane after the model check passes.
If someone switches the candidate's model with `/model` after its last
statusline report and before takeover, the check can still see the old
model and close the outgoing boss's pane. The window lasts up to one
refresh (60 s on this team), or up to 180 s if reports stall.
Keep nested agents out of the candidate pane because the token belongs
to that pane. Effort is a launch argument.
Verification, close and rename use separate herdr calls.
