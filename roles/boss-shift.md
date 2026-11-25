<!-- boss-shift-role: 3 -->
# Boss shift

“No summary no blind hand off a proper hand off like a shift worker.”

The outgoing boss runs the team through review and alone gives GO. The shift's
attempt directory holds the record. `DIR` is its absolute path and `ID` its
name; the team script's `brief` gives both and points to the checked role copy
in that directory. Use the path in the cue because the `current` link can name
another attempt by the time you read it. The written rounds are advice for
people. GO is gated on the approval record that the outgoing boss writes with the
team script's `approve` (below), and `go` also proves the candidate's model.

## Candidate

1. Verify your model from your own session and the shift's `status`. If proof
   is absent or the model differs, put the problem in the attempt directory and
   wait. Keep nested agents out of the review so their reports cannot replace yours.
2. Read the team's working rules and `STANDING.md` in the parent of your attempt
   directory. Follow its pointers to the shift board, owner questions and past
   corrections. Verify workers and their obligations, PR heads and checks, the
   inbox, lanes and other live sources. Record any divergence and its source.
3. Publish round 1 with the team script's `account 1`, the body on stdin: workers
   and context, PR and branch owners and blockers, owner requests, surprising
   rules, five first actions and questions. The script takes the shift's lock, so
   an account never lands while the takeover runs; a file renamed in by hand takes
   no lock and is not covered. After GO the account is final.
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

Approve the newest account with `approve N SHA256`, where SHA256 is the `sha256`
that `wait` printed with the text you reviewed: it is refused if the account is no
longer those bytes, or if a later round exists. `go` needs that approval and writes
nothing when it refuses, so the same approval serves once the model is right. To
withdraw it, even after GO, run `withdraw`: it is final, `takeover` refuses, and
the shift is opened again. A candidate that rewrites its account after your
approval is refused by `go` and by `takeover`; approve the new bytes if no GO is
recorded.

Before GO, stop and join the board reader, inbox watcher and timers. Record
their final output in an atomic `cutoff.md` with receipt pointers, the last
read board sequence and outstanding work. Nothing enforces the cutoff or the review:
they are your procedure. Use the script's status on an ambiguous GO result, and
run `go` again after a failed cue. Resume consumers only on a definite refusal.

## Publication

The script's `account N` writes an account (UTF-8, not blank, at most 120 KiB,
checked before the lock is taken). A reply is written by hand: UTF-8 Markdown, at
most 128 KiB, to a temporary file in the attempt directory, then renamed to
`reply-N.md`. What the reader accepts, for reference (N from 1 to 3):
begin with `attempt: ID` and `round: N`. A reply continues with
`decision: ready` or `decision: revise` and `account: SHA256` (64 hex digits),
each on its own line. Leave a blank line before the body. End with
`<!-- end shift round -->` on its own line and a final newline. A file that
does not follow this exactly is not a publication.
