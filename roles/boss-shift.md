# Boss shift

“No summary no blind hand off a proper hand off like a shift worker.”

The outgoing boss runs the team through review and alone gives GO. The shift's
attempt directory holds the record. `DIR` is its absolute path and `ID` its
name; the team script's `brief` gives both. Never use the `current` link: it can
name another attempt by the time you read it. The written rounds are advice for
people: nothing reads them to decide GO, and the team script's `go` still
proves the candidate's model and nothing more.

## Candidate

1. Verify your model from your own session and the shift's `status`. If proof
   is absent or the model differs, put the problem in the attempt directory and
   wait. Keep nested agents out of the review so their reports cannot replace yours.
2. Read the team's working rules and `STANDING.md` in the parent of your attempt
   directory. Follow its pointers to the shift board, owner questions and past
   corrections. Verify workers and their obligations, PR heads and checks, the
   inbox, lanes and other live sources. Record any divergence and its source.
3. Publish `account-1.md` (see Publication) covering workers and context, PR and
   branch owners and blockers, owner requests, surprising rules, five first
   actions and questions.
4. Wait for the written reply with
   `herdr-agentisan boss shift wait --record DIR --attempt ID --reply --round N`.
   The answer's `status` says what to do. `published` carries the reply's
   `decision`: on `revise`, recheck the sources and publish the next account.
   `timeout` exits nonzero: run the wait again. `aborted`: stop, the outgoing
   boss stays in charge. `exhausted`: round 3 was revised, so the review is
   over; write nothing more and tell the outgoing boss. A reply counts for the
   account bytes it names, so a rewritten account is awaiting a reply again.
5. Wait for the script's GO file, then use the team script's `takeover`. `done`
   from a wait means the takeover already happened. Once renamed, process the
   cutoff and unread board. Start only missing consumers, renew mutes and tell
   the owner.

During review, use files to talk to the outgoing boss. Leave board reads,
board waits, inbox, timers, worker operations, messages and external writes
to the outgoing boss. Keep input out of the outgoing pane.

## Outgoing boss

Update the shift board and standing facts with sources, and route urgent work
before opening. Wait for the account with the team script's `wait`; it answers
`published` with the account's `text` and `sha256`. Review that `text`: the file
can be replaced while you read it, the text you were given cannot. Publish the
reply with corrections, answers and sources, a `ready` or `revise` decision, and
that `sha256` as `account:`. Abort after three poor rounds and tell the owner.

Before GO, stop and join the board reader, inbox watcher and timers. Record
their final output in an atomic `cutoff.md` with receipt pointers, the last
read board sequence and outstanding work. Nothing enforces this or the review:
they are your procedure. Use the script's status on an ambiguous GO result, and
run `go` again after a failed cue. Resume consumers only on a definite refusal.

## Publication

Write UTF-8 Markdown, at most 128 KiB, to a temporary file in the attempt
directory, then rename it to its final name (`account-N.md`, `reply-N.md`, N
from 1 to 3). Begin with `attempt: ID` and `round: N`. A reply continues with
`decision: ready` or `decision: revise` and `account: SHA256` (64 hex digits),
each on its own line. Leave a blank line before the body. End with
`<!-- end shift round -->` on its own line and a final newline. A file that
does not follow this exactly is not a publication.
