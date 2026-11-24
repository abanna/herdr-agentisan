# Boss shift

“No summary no blind hand off a proper hand off like a shift worker.”

The outgoing boss runs the team through review and alone gives GO.

## Candidate

1. Verify your model from your own session and the shift's `status`. If proof
   is absent or the model differs, put the problem in the attempt directory and
   wait. Keep nested agents out of the review so their reports cannot replace yours.
2. Read the team's working rules and `STANDING.md` in the parent of your attempt
   directory. Follow its pointers to the shift board, owner questions and past
   corrections. Verify workers and their obligations, PR heads and checks, the
   inbox, lanes and other live sources. Record any divergence and its source.
3. Publish `account-1.md` covering workers and context, PR and branch owners
   and blockers, owner requests, surprising rules, five first actions and questions.
4. Wait for the written reply with
   `herdr-agentisan boss shift wait --record DIR --attempt ID --reply --round N`.
   Re-arm on timeout. On `revise`, recheck the sources and publish the next
   account. After a model correction, recheck your model and publish a fresh
   account for review. A `ready` reply approves one round.
5. Wait for the script's GO file, then use the team script's `takeover`.
   Once renamed, process the cutoff and unread board. Start only missing
   consumers, renew mutes and tell the owner.

During review, use files to talk to the outgoing boss. Leave board reads,
board waits, inbox, timers, worker operations, messages and external writes
to the outgoing boss. Keep input out of the outgoing pane.

## Outgoing boss

Update the shift board and standing facts with sources, and route urgent work
before opening. Check the account and reply with corrections, answers, sources
and a `ready` or `revise` decision. Abort after three poor rounds and tell the owner.

Before GO, stop and join the board reader, inbox watcher and timers. Record
their final output in an atomic `cutoff.md` with receipt pointers, the last
read board sequence and outstanding work. Use the script's status on an
ambiguous GO result. Resume consumers only on a definite refusal.

## Publication

Write UTF-8 Markdown to a temporary file in the attempt directory, then
rename it to its final name. Begin with `attempt: ID`, `round: N`, and for
replies `decision: ready` or `decision: revise`, each on its own line.
Leave a blank line before the body. End with `<!-- end shift round -->`
on its own line and a final newline.
