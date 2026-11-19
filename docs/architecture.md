# Architecture

## The shape

Two binaries:

```
cmd/herdr-agentisan ─→ internal/cli    (cobra tree herdr invokes from the manifest)
                              └─→ internal/<domain> packages (the rules)
cmd/devctl          ─→ internal/devctl (the repository's own gates)
```

`cmd/*` packages are thin shims — config, logging, tracing, signal handling,
fang styling. No logic. That is what keeps them outside the coverage
denominator honest: there is nothing in them worth testing that running the
binary does not already prove.

The load-bearing decision is that **rules live in domain packages under
`internal/`, not in commands**. herdr reaches the plugin through several
doors — actions, event hooks, startup hooks, panes — and each is a command;
a rule written into one of them is a rule the others do not have.

## How herdr runs the plugin

herdr reads `herdr-plugin.toml` and, for each action, execs one argv array
with the plugin root as the working directory and no shell. The process gets
its context through environment variables (`internal/plugin.EnvFrom`) and
talks back over herdr's unix socket (`internal/herdr`): newline-delimited
JSON, one request per connection. Every call is bounded by
`plugin.CallTimeout` (5s), because the process context carries no deadline
of its own and a herdr that accepts and never answers would otherwise hold
one of its in-flight action slots open indefinitely.

Two properties are enforced rather than hoped for:

- **The manifest matches the binary.** `herdr plugin link` runs no build and
  herdr only warns about a bad manifest, so `TestManifestMatchesTheBinary`
  checks that every action names a runnable cobra command exactly and execs
  the path the build writes.
- **Tests never reach a live herdr.** A developer's shell inside herdr has
  `HERDR_SOCKET_PATH` set. Commands read the environment through an injected
  lookup, and tests dial `herdrtest`, an in-process fake socket.

## Why two CLIs

`herdr-agentisan` is the product. `devctl` is the repository's own tooling, and it
exists because three of this repo's gates have no Go builtin:

- **Coverage floor.** `go test` has no `--cov-fail-under`. `devctl coverage`
  parses `go tool cover -func` and fails below the threshold.
- **Docs parity.** `devctl docs-parity` proves AGENTS.md, `Taskfile.yml` and
  `ci.yml` describe the same gates, and that no escape-hatch entry in
  `internal/devctl/parity.go` outlives the task or row it exempted.
- **Large files.** `devctl large-files` fails on a tracked file above the size
  ceiling, which git itself never enforces.

Keeping these in Go rather than a shell or Python script means the repository
needs no second language to run its own checks, and the checks are themselves
tested.

## The drift guard

An agent's most expensive failure mode here is not writing a bug — it is
trusting a document that has quietly stopped being true. `devctl docs-parity`
exists specifically to make that impossible: a documented command that CI no
longer runs would otherwise be run by an agent, seen to pass, and taken as
proof the gate passes.

It was verified by deliberately breaking it and watching it fail before being
trusted — a check that has never been seen failing is not evidence of anything.

## Configuration

`internal/config` resolves flags > environment > `.env` > defaults, and
validates. No secret has a default; when one is added, `config.Load()` must
fail at startup rather than fall back. Failing early is the design, not a
rough edge.

## GitHub notice capture IDs (B0a.1)

`store.GitHubNoticeCaptureID` is a pure operation over frozen `NoticeEnvelope`
and `NoticeReceipt` values. It preserves inputs, ignores the caller's CaptureID
entirely and returns `gh1:` plus lowercase SHA256 hex, or a wrapped ErrInvalid
without input contents. It reads no state, clock or environment and has no
production callers. It allocates no capture identity, admits no record and
performs no board, seen, acknowledgement, delivery or wake effect.

Version 1 accepts only source github and merged/ci_failed/ci_passed/dirty/
approved/review, with a matching version-1 receipt and PR 1..2^31-1. Project,
subject and worker allow empty, otherwise UTF-8 without control/format characters,
at most 256 bytes. Text is nonempty UTF-8, at most 512 bytes, without NUL/CR/LF.
Repo has two nonempty components and one slash, at most 256 bytes, with the same
Unicode restrictions. OIDs are full 40/64 hex characters, preserving case;
approved keys are empty, and merge keys may be empty. Review keys are a login
(at most 128 bytes, no whitespace/control/format) plus one ASCII space and
a canonical UTC RFC3339Nano timestamp, together at most 256 bytes. The login is
empty for a deleted account's review, whose key is the space and time alone
(B0a.1b): only keys B0a.1 refused are newly accepted, so no existing ID changes.
A conformance test builds every event the notice constructors make, at GitHub's
own bounds, into a valid envelope; inputs past them are refused. Those inputs
are bounded where they enter (NERD-5632): `config resolve` refuses a `github`
past 245 bytes, so `repo#pr` fits the subject with any PR number, and a project
name past 256, each with `settings.ErrTooLong`; `ParseOpenPRs` refuses a PR
number past 2^31-1 as `ErrOutput`. A test at those bounds fails when they and
the envelope drift apart. What the daemon does with a notice admission refuses
is slice B1's.

Encoding is four unsigned 64-bit big-endian words: envelope version, receipt
version, PR, then the signed nanosecond instant's two's-complement bits. The instant
uses the existing representable nanosecond range (1677..2262), rejecting Go zero
time, with UTC normalization. Next come unsigned 32-bit big-endian lengths and
bytes for project/source/kind/subject/worker/repo/key/text in that exact order.
CaptureID and result/retry/disposition data are absent. No text is rendered,
truncated or normalized. Length guards precede variable decoding/error allocation.
The 2-KiB encoding ceiling is enforced; a conservative valid bound is 32 numeric
bytes + 32 prefixes + five 256-byte fields + 512 text + 6 source + 9 kind = 1871,
so fully valid inputs cannot reach 2048. Tests cover reachable field boundaries.

## The notice admission ledger (B0a.2)

Migration 0009 adds the ledger that will record each notice once, before the
board tells it: `notice_identities`, permanent, and `notice_payloads`, the
frozen envelope of each identity not yet settled. It is dormant. Nothing in
production calls `store.AdmitNotice` or `store.PendingNotice`, neither posts
to the board nor wakes `board wait`, and neither writes a `github_*` table;
`TestLedgerHasNoProductionCaller` fails on any use of the ledger's names, or
B0a.1's, in a non-test file of `internal/` or `cmd/` but `noticeledger.go`,
`noticequota.go` and `boardnotice.go`, and on any string naming a ledger
table, in any case, in one but `noticeledger.go` and `noticequota.go`. It also
fails on any non-test Go string that names a ledger table and contains
REPLACE. The migration is one-way: an older build
refuses the database (`ErrSchemaTooNew`), so no build holding it is installed
before the board's activation, and the first install is the activation build.

`AdmitNotice` takes only a GitHub envelope, with a project, whose CaptureID is
the one `GitHubNoticeCaptureID` derives; anything else is ErrInvalid. In one
transaction it then dedupes, by capture ID and by receipt (project, repo, PR,
kind, key), which the schema also holds `UNIQUE`: the capture ID covers the
time observed and the text, so the same event seen after a restart is caught
by its receipt, and the first capture wins, its payload never rewritten. An
event `github_seen` already holds, compared exactly (BINARY: no case folding,
no prefix), was told on the typed path: it is recorded `imported`, with no
payload, and `github_seen` is only read. Anything else is recorded `pending`
with its payload: its canonical encoding's size and SHA-256. `PendingNotice`
returns a pending notice's envelope only after recomputing its capture ID,
digest and size from the rows; a row edited by hand is ErrStore, never handed
on. No error carries the notice's content.

The schema refuses on its own what the ledger must never hold, whoever
writes it, and its test pins each refusal to the one constraint that makes
it. A GitHub identity has a project and a repo, as `github_seen` does, so it
can always be settled there, and a digest is 64 lowercase hex bytes with
nothing after them: SQL's `length()` and `GLOB` stop at a NUL, so the CHECK
also counts its bytes.

A dropped row gives its reason: `skipped`, `muted` or `repeat`, or one of
two terminal reasons, so no notice stays pending for ever and counts against
the quotas. `failed` is a notice that could not be posted: its retries ran
out, or its stored copy no longer verifies. `expired` is one whose team or
project is gone, or that is too old. Nothing writes either yet: B0b decides
when each is written (deviation (g), decided by the user).

The schema already holds every source (`github`, `daemon`, `team`), every
state (`pending`, `imported`, `posted`, `dropped`) and the settle columns, so
other sources' allocated IDs and the payload prune (B0a.4) need no migration.
That holds for the GitHub and daemon sources: a `team` post's author has no
column yet. The quotas (B0a.3-limits) need one, 0010, for their running
totals.
The board's writes (B0b onward) and its activation remain, each after human
merge and separate authorization.

## The notice ledger's totals (B0a.3-totals)

Migration 0010 keeps each project's and the store's identity totals by
trigger, in `notice_counts` and `notice_store_counts`, backfilled from 0009's
rows. It follows a delete or a project move rather than refusing it, since
permanence is B0b's, and keeps a project's row at 0 once it is empty. A
REPLACE's hidden delete is followed only on a connection with
`recursive_triggers` on. The store sets it on every connection and refuses to
open without it (`ErrPragma`). On any other connection, a sqlite3 shell
included, a REPLACE leaves the totals high until the recount below, and B0b's
guard triggers close REPLACE everywhere.

The totals serve the identity quotas Q6 (100,000 a project) and Q7 (500,000
in the store): each check reads one row instead of counting
`notice_identities` under the write lock. Q6 and Q7 read them, in the
admission's transaction.

Only a hand edit, or a REPLACE on another connection, makes the totals drift,
and this recount repairs them, in one transaction (it drops the kept zero
rows, which is harmless):

```sql
BEGIN IMMEDIATE;
DELETE FROM notice_counts;
INSERT INTO notice_counts (project, identities)
    SELECT project, count(*) FROM notice_identities GROUP BY project;
DELETE FROM notice_store_counts;
INSERT INTO notice_store_counts (id, identities)
    SELECT 1, count(*) FROM notice_identities;
COMMIT;
```

0010 is one-way, so the install rule stands: no build holding it is installed
before the board's activation. It is dormant: only 0010's triggers write the
totals, and `TestLedgerHasNoProductionCaller` fails on any non-test Go string
naming them outside `noticeledger.go` and `noticequota.go`. The connection
setting is the one live change.

## The notice quotas (B0a.3-limits)

`noticequota.go` refuses an admission past one of eight quotas; it never makes
room by evicting what was admitted. Q1 to Q3 count pending notices: 1,000 a
project, 5,000 in the store and 32 a pull request. Q4 and Q5 sum their bytes,
as `notice_payloads.bytes` stores them: 8 MiB a project and 32 MiB in the
store. Q6 and Q7 count notices in every state: 100,000 a project and 500,000
in the store. Q8 counts a project's admissions, in every state: 600 an hour.

A pending notice faces all eight, an import (an event `github_seen` already
holds) Q6 to Q8, and a duplicate or an invalid envelope none, since
`AdmitNotice` returns both before the quotas run. So a duplicate stays a
duplicate however full the ledger is.

Each refusal is its quota's own error, which wraps `ErrNoticeQuota`. It names
only the quota and its limit, never the notice, and writes nothing: the
transaction rolls back, and the totals with it. Which quota is reported when
several are full is not part of the contract.

The checks run in the admission's transaction, after the dedupe and the
`github_seen` lookup, on the store's one connection, so no admission overtakes
another between a count and its insert. Each reads at most its cap's rows,
however large the ledger grows. Q8's window is one hour, bounded below only:
an admission exactly an hour old is out, and one stamped after the admission
being checked counts.

Q6 and Q7 read the totals of the section above. A missing store total, or a
missing total of a project that has identities, is `ErrStore`; a project with
neither reads 0. The recount above repairs a missing total.

It is dormant: `TestLedgerHasNoProductionCaller` covers `noticequota.go`, its
names and its SQL.

## worker spawn's retry keys (NERD-5254)

The retry keys' migration adds the store a retried `worker spawn` will answer
from (ADR-001 A73). `spawn_requests` holds one record per retry key: the
request it was first used for, as asked, `running` until its answer is stored,
then `done` with it. `spawn_request_workers` holds the ids that request
planned, in order, each with the generation it was bound at once bound. A link
pins an id and its generation, so an answer can tell a worker retired since
from one whose id was bound again; a column on `workers` could not, since a
reused id rebinds its row.

They are dormant until a keyed `worker spawn` reaches them (A73: the socket
refuses a key until part F). `BeginSpawn` writes a record and its planned ids
in one transaction, and the insert itself refuses a known key
(`ErrSpawnKeyExists`), never a read before it, so no replace can pass
unnoticed. `SpawnRecord` reads one with each planned id's `workers` row now,
while still at its linked generation, and its briefs: the messages to it at
that generation from a boss or a pane, in any state. `DropSpawn` deletes a
running record none of whose ids is bound.
`TestSpawnKeysHaveNoProductionCaller` fails on any use of their names in a
non-test file of `internal/` or `cmd/` but `spawnkeys.go`, on any string
naming their tables outside it, and on any naming one with REPLACE.

The schema refuses on its own what a record must never hold, whoever writes
it: a key, effort, digest or outcome off its grammar, each compared by its
byte length too, since SQL's `length()` and `GLOB` stop at a NUL; a count
outside 1 to 64; a done record without its time or its answer; an answer
that is not JSON. The migration is one-way and runs at `Open`: an older build
then refuses the database (`ErrSchemaTooNew`).

## Deliberate limits

- **Tracing exports only when told to.** `HERDR_AGENTISAN_OTLP_ENDPOINT` is empty by
  default, so `task run:cli` on a laptop records no spans and needs no
  collector. W3C propagation is installed either way, so a trace context still
  passes through to the next hop. The daemon's workflow events follow the same
  rule: no `[telemetry] endpoint` and no variable, nothing is built
  (`docs/telemetry.md`).
