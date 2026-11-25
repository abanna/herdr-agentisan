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
writes `model_session`. Check the preimage SHA-256, then dry run and apply. `--forward` makes a second run fail
where plain `patch` would take the applied patch for a reversed one and undo it.

```sh
script=~/.local/share/ha-team/boss-shift/shift.sh
printf '%s  %s\n' a9d92216ec0324c87372ed9fb38579568a57dcf72460d4e9366098abc3d38402 "$script" |
  sha256sum --check &&
  patch --batch --forward --dry-run --fuzz=0 "$script" < docs/boss-shift-v1.patch &&
  patch --batch --forward --fuzz=0 "$script" < docs/boss-shift-v1.patch
```

## Written rounds

The candidate and the outgoing boss review in files in the shift folder, by the
[role](../roles/boss-shift.md): `account-N.md`, then `reply-N.md`, N from 1 to 3,
each naming the attempt (the folder's name), its round and an end marker. A
reply adds `decision: ready|revise` and `account: <sha256>`. Replies guide the
review. The approval gate below checks the newest account against the approval
record. The verbs read only the folder they are given,
bounded and strict:

```sh
herdr-agentisan boss shift brief --record "$dir" --attempt "${dir##*/}" --role "$role"
herdr-agentisan boss shift wait --record "$dir" --attempt "${dir##*/}" [--round N] [--reply] [--timeout 2m]
```

`wait` answers `published`, `timeout` (nonzero), `aborted`, `done` or `exhausted`
(round 3 was revised). An account verdict carries the account's `text` and the
`sha256` of exactly that text: the reviewer reads the text, not the file, and a
reply names that digest. A reply counts only for the bytes it names. Without a
round, `wait` returns the first account that has no such reply, so a round
published after a refusal is found. A reply is read before the account it must
name, but the poll is not locked: a verdict can briefly describe a pair that
changed a moment later, which the next poll corrects.

The script's `brief` sends the role's path and `wait` calls the verb. Install
after v1, with a build that has both verbs (`SHIFT_ROLE` names a role file
outside the script's repo); the patch changes only the script's `brief`, `wait`
and `open` lines, so it applies to an installed script whose model default is
edited:

```sh
script=~/.local/share/ha-team/boss-shift/shift.sh
patch --batch --forward --dry-run --fuzz=0 "$script" < docs/boss-shift-v2.patch &&
  patch --batch --forward --fuzz=0 "$script" < docs/boss-shift-v2.patch
```

A script that is not v1's output, or has v2 already, is refused by the dry run.
v1's stock output has SHA-256
`ce75b6c4d4cd9d666e259dc41f8623ff9b8537696e504407941d59e5055fb1c1`. The patch
also stops a lowercase `started` in the caller's environment from failing a good
`open`. Cutoff and consumer quiescence are the role's procedure, not enforced here.

## Approval gate

The outgoing boss's approval is ONE record, `approval` in the shift folder, written
only by `shift.sh approve N SHA` and `shift.sh withdraw`, under the shift lock, by
tmp file and rename: `state: approved|withdrawn`, `round`, `account` (the SHA-256
of account-N.md) and `at`. `SHA` is the `sha256` that `wait` printed with the
text the boss read; `approve` refuses when round N has no valid account, when the
account is not those bytes now, when a later round exists, after GO, and after a
withdrawal. `withdraw` is for good and works at any time, even after GO: the
shift is then aborted and opened again.

`go` proves the record against the files as they are (approved, the newest
account is round N with the recorded digest) and then the candidate's model, and
a refusal writes nothing, so the same approval serves once the cause is fixed.
`takeover` proves the record again as its last step before the close, after its
own pane lookup and model proof: a rewritten account, a round added, a
withdrawal, or a missing record stops it, even after GO, and the answer is to
abort and open again. The approval checks and writes share the shift lock.
The candidate publishes an account only with `shift.sh account N` (the
body on stdin, read and checked before the lock is taken, so a stalled writer
never holds up `takeover` or `abort`): it lands before `takeover`'s check, which
then sees it, or waits until the shift is over and is refused; after GO or a
withdrawal it is refused. What remains: an account file renamed in by hand, which
takes no lock.

```sh
script=~/.local/share/ha-team/boss-shift/shift.sh
patch --batch --forward --dry-run --fuzz=0 "$script" < docs/boss-shift-v3.patch &&
  patch --batch --forward --fuzz=0 "$script" < docs/boss-shift-v3.patch
```

Install it after the v2 patch; it applies only to v2's output. Install the current
`roles/boss-shift.md` with it (or point `SHIFT_ROLE` at the current copy).
`brief` copies the role into the attempt directory and checks that the copy has
the line `<!-- boss-shift-role: 3 -->`, which marks the procedure that uses
`account N`. It sends the checked copy's path. Each cue gets its own copy so a
checkout change or another brief leaves the earlier cue's instructions intact.

## What each command does

- `open` makes a shift folder of its own, `<root>/YYYYMMDD-HHMMSS-XXXXXX`, and
  claims it by making `current` a link to it, so an aborted open can never
  write into its replacement's folder. It records the old terminal, launch time
  and session id, then splits, saves the candidate's pane and terminal, and
  starts Claude with that id. It reads no model. If the shift ended during the
  split (`current` gone, or `ABORTED`), or was aborted while Claude started, it
  closes the new pane and stops.
- `brief` checks the role copy, saves the Claude's pid and start once, then
  proves it and its model on every run before sending the instructions.
- `go` and `takeover` prove the same again. `takeover` looks up the old pane's
  terminal first, proves the candidate, then checks the approval before it
  closes the pane. `abort` verifies the candidate's terminal before closing its
  pane in an active shift.
- `status` shows the record and the verdict; it writes nothing. `brief`, `say`,
  `go`, `abort` and `takeover` wait 30 s for the shift's lock, then say "the
  shift is busy; try again"; `open` and `wait` take no lock.

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

The owner accepted one window: a `/model` switch, or a `/clear` (a new session
id in the same process), after the candidate's last statusline report and before
`takeover` is not seen, so the check can still close the outgoing boss's pane.
It lasts until a report of the new session is the last one written (a refresh,
60 s here), after which every check refuses `wrong_session`; if reports
stall, the last one expires after 180 s and, once herdr drops it, every check refuses `missing_model` (`stale_report` is a report still there but older than the launch, the process start or 180 s). A report of the old session still in flight can be written
after it, because herdr keeps the last write, and reopens the window. Verification, close and rename
are separate herdr calls, and a pane can change between the terminal check and
the close. The session proof assumes the statusline's `session_id` equals
`--session-id` (herdr's own `agent_session` does); if it differs, every check
refuses: replay once at activation, with a `/clear`.
