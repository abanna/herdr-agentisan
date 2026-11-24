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
Record the returned `pid` and `start` when both are nonzero, along with
the current Unix milliseconds as `--pinned-at`. Pass the three values
on subsequent checks. Verification requires a report strictly after
that pin time plus one second. Inspect `requested_model`,
`observed_model` and `reason` to diagnose a refusal.

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

Use `shift.sh status` for the current verdict. If proof is missing or
stale, wait for a fresh statusline report, repair the hook, or abort.
If open was interrupted before pinning, use `shift.sh status` to record
the candidate and wait for a later report on that same process. The `SHIFT_MODEL_SEEN`
instruction in the old outgoing checklist is obsolete. GO requires verified model proof. If a post-GO check fails,
the candidate must restore verified proof before retrying takeover.
If GO was recorded but cueing failed, rerun `shift.sh go` from the
outgoing pane to recheck the candidate and resend the cue. The script keeps
the original GO and records a successful cue in `GO-CUED`. A failed
prompt response can be ambiguous about whether the cue arrived.

The statusline token belongs to the pane. Keep nested agents out of the
candidate pane and verify a model switch after the next statusline report.
Effort is a launch argument. The script still owns the shift files and
makes separate calls to herdr for verification, close and rename.
