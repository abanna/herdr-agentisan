# Boss shift helpers for the team script

NERD-5262 moves model readback and written review into the plugin. Between
attempts, install the binary and [role](../roles/boss-shift.md) together. Keep
[standing facts](config/boss-shift-standing.example.md) in `STANDING.md` beside
the script. Update the outgoing checklist to follow the shipped role, including
stopping consumers before GO and removing the manual model override.

The team boss applies [the v1 adapter](boss-shift-v1.patch) from the script
directory. Verify `sha256sum shift.sh` matches
`a9d92216ec0324c87372ed9fb38579568a57dcf72460d4e9366098abc3d38402`.
Dry-run `patch --dry-run --fuzz=0 shift.sh < /path/to/boss-shift-v1.patch`,
then repeat without `--dry-run`. The adapter uses GNU `date`. Set
`SHIFT_PLUGIN` for a different installed binary path or `SHIFT_ROLE` for
an absolute role path outside the script's repo.

- `check-model --pane P --terminal T --name NAME --model ID --launched-at MS`
  reads the recorded terminal. The adapter saves any observed Claude PID and
  raw start in `current/state`, then adds `--pid` and `--start` at GO and
  takeover. JSON carries `requested_model`, `observed_model`, `reason`, `pid`
  and `start`. Only `verified` exits zero. IDs match exactly, with `fable` also
  accepting numeric `claude-fable-MAJOR-MINOR` IDs and an optional date suffix.
- `brief --record DIR --attempt ID --role FILE` returns `record`, `attempt`
  and `role`. Pass absolute paths and the attempt directory's name.
- `wait --record DIR --attempt ID [--round N] [--reply | --ready] [--timeout 2m]`
  returns `status`, `attempt`, `round` and, when published, `path` and any
  `decision`. `--reply` needs a round. GO uses `--ready` for the latest
  account's approval. A model refusal invalidates both reviewed files. Waits
  accept zero for one probe and run for at most five minutes. A timeout exits
  nonzero. An abort returns `aborted`. See the role for publication.

The script's files remain authoritative and depend on cooperative writers.
Model proof can lag a switch or reflect a nested report. Linux rounds process
starts down, and effort is requested in argv. Separate herdr calls leave
an interval between verifying a target and using it.
